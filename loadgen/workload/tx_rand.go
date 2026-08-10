/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package workload

import (
	"crypto/sha256"
	"encoding/binary"

	"github.com/hyperledger/fabric-x-common/common/crypto"
)

// txRandomProcess is the deterministic content model behind the transaction stream. It derives every
// random part of a transaction: the key of each slot (by its flat key index) and its value (by the key
// index and the writing transaction, so updates change the value), plus the nonce (hence the TX ID) and
// metadata of the transaction (by the transaction index). It also
// owns the per-transaction slot layout — which slots create keys and which reference existing keys,
// plus their key indices — as a float-rate function of the transaction index. It embeds the
// TransactionProfile (key/value sizes, slot counts, and the split configuration).
// One process is created per worker generator (it
// is driven from a single goroutine), so it reuses a preallocated seed buffer across derive calls;
// worker-count invariance comes from the shared atomic transaction index, not from sharing this. Only
// the ECDSA signature and the envelope timestamp — added by the builder — are non-deterministic and
// outside this process.
type txRandomProcess struct {
	TransactionProfile
	// seed is the PRF input buffer, laid out (rootSeed | domain | index | subIndex | block) and reused
	// across calls: the rootSeed prefix is written once at construction; derive rewrites the domain
	// byte and the two index fields.
	seed [addressLen]byte
	// selectedSet is the reused membership set for Floyd's algorithm, which samples a transaction's backward
	// references as distinct offsets into the lookback window — a uniform sample without replacement. It is
	// cleared and refilled per transaction to avoid a per-transaction allocation; this is safe because the
	// process is driven from a single goroutine.
	// For more information on Floyd's algorithm: https://dl.acm.org/doi/pdf/10.1145/30401.315746
	selectedSet map[int64]struct{}
}

// txSlotKeys holds the flat key index for every slot of a transaction, split by role. Indices are
// signed: non-negative indices live in the committable/introduced space; negative indices are
// pre-genesis warmup keys (an existing reference that would point below index 0 early in the run),
// never produced by a non-negative new-create.
type txSlotKeys struct {
	readOnly   []int64
	readWrite  []int64
	blindWrite []int64
}

const (
	// Derivation domains. The leading byte of the PRF input separates the byte streams, so the same
	// index yields independent key, value, nonce, and metadata bytes.
	domainKey      = 'K'
	domainValue    = 'V'
	domainNonce    = 'N'
	domainMetadata = 'M'
	domainSign     = 'S'
	domainRef      = 'R' // per-transaction random offset into the backward reference window.

	// addressLen is the length of the buffer hashed to derive content, laid out rootSeed(8) +
	// domain(1) + index(8) + subIndex(8) + block(4): the rootSeed prefix is fixed per run, the domain
	// byte separates the streams, each content type is addressed by one or two indices (key by key index;
	// value by key index + writing tx index; nonce/metadata by tx index), and the trailing block counter
	// drives the MGF1-style expansion — so a single buffer is hashed per block, with no separate counter
	// array.
	addressLen = 8 + 1 + 8 + 8 + 4
	// domainOffset, indexOffset, subIndexOffset, and blockOffset locate the per-call fields within the
	// reused buffer.
	domainOffset   = 8
	indexOffset    = 9
	subIndexOffset = 17
	blockOffset    = 25
)

func newTxRandomProcess(profile *Profile) *txRandomProcess {
	t := profile.Transaction
	p := &txRandomProcess{
		TransactionProfile: t,
		selectedSet:        make(map[int64]struct{}, t.ReadWriteCount+t.BlindWriteCount+t.ReadOnlyCount),
	}
	//nolint:gosec // reinterpret bits: the seed is an opaque PRF input.
	binary.BigEndian.PutUint64(p.seed[0:domainOffset], uint64(profile.Seed))
	return p
}

// key returns the key bytes at the given flat key index. The index is signed (negative = pre-genesis
// warmup key); its uint64 bit pattern keys the PRF, so every distinct index yields distinct key bytes.
func (p *txRandomProcess) key(keyIndex int64) []byte {
	return p.derive(domainKey, uint64(keyIndex), 0, p.KeySize) //nolint:gosec // reinterpret bits.
}

// value returns size value bytes for key keyIndex as written by transaction txIdx. Unlike the key, the
// value is a function of BOTH the key and the writing transaction, so every write to a key — including a
// later update — produces distinct value bytes (a real state change), while staying reproducible from
// (txIdx, keyIndex).
func (p *txRandomProcess) value(txIdx uint64, keyIndex int64, size uint32) []byte {
	return p.derive(domainValue, uint64(keyIndex), txIdx, size) //nolint:gosec // reinterpret bits.
}

// metadata returns size metadata bytes for the transaction at index txIdx, or nil when size is 0.
func (p *txRandomProcess) metadata(txIdx uint64, size uint32) []byte {
	return p.derive(domainMetadata, txIdx, 0, size)
}

// nonce returns the nonce for the transaction at index txIdx; the TX ID derives from it.
func (p *txRandomProcess) nonce(txIdx uint64) []byte {
	return p.derive(domainNonce, txIdx, 0, uint32(crypto.NonceSize))
}

// slotKeys decides which key each slot of transaction txIdx touches. Reads and writes draw from one shared
// pool of keys, and a key only comes into existence when a write slot creates it. There is no map of live
// keys: the generator tracks a single running count of how many keys have been introduced so far (the
// committed frontier) and derives every slot from that count and the transaction index.
//
// Each transaction introduces some fresh keys first, then fills its remaining slots with backward
// references to keys introduced earlier. How many fresh keys it introduces is how far the frontier advances
// over this one transaction (capped at the slot count); the slots left over become references. Fresh slots
// are filled first in layout order — read-write, then blind-write, then read-only — so a write creates its
// key while a fresh read-only slot reads a key nothing ever creates: a harmless wasted index (only writes
// advance committed state, but the frontier still counts every index it hands out). This is the whole point
// of the knob: a higher backref rate means fewer fresh keys and more reuse, which is what produces the
// commit-time contention.
//
// A backward reference points below the frontier as it stood some transactions earlier — the reference gap,
// which chooses between keys still in flight (small gap) and keys already committed (large gap). Within a
// lookback window the references are spread across the newest keys of that window as a uniform sample
// without replacement, so a wider window dilutes contention and a narrower one concentrates it. References
// that do not fit the window — and every reference when no window is set — step straight back one key at a
// time from the top of the window; that is the most-contended pattern, and because it needs no room it is
// why no window is ever too small.
//
// Fresh keys and references can never land on the same index, and a transaction's own references are always
// distinct. Early in the run (and for the entire run when there are no fresh keys at all) references reach
// below key index zero; those negative indices are just distinct pre-genesis keys no create ever produces,
// so they are left alone rather than clamped. At the default backref rate of zero there are no references
// at all — every slot gets its own fresh key, the original contention-free workload.
func (p *txRandomProcess) slotKeys(txIdx uint64) txSlotKeys {
	slotsPerTx := int64(p.ReadWriteCount + p.BlindWriteCount + p.ReadOnlyCount)
	newKeysRate := float64(slotsPerTx) - p.KeyBackrefRate
	frontier := committedFrontier(txIdx, newKeysRate)
	newKeys := min(slotsPerTx, committedFrontier(txIdx+1, newKeysRate)-frontier) // in [0, slotsPerTx]
	allKeys := make([]int64, slotsPerTx)
	fillNewKeys(allKeys, frontier, newKeys)

	nRefs := slotsPerTx - newKeys
	window := int64(p.KeyLookbackWindow) //nolint:gosec // key-lookback-window config value fits int64.
	top := committedFrontier(txIdx-min(txIdx, p.TxReferenceGap), newKeysRate)
	sampled := min(window, nRefs)
	rnd := p.derive(domainRef, txIdx, 0, uint32(8*sampled)) //nolint:gosec // small slot count fits uint32.
	clear(p.selectedSet)
	// Floyd's algorithm picks distinct offsets into the window, spread uniformly, in a single pass — so we
	// neither allocate the whole window nor shuffle it just to draw a few references from it.
	for i := range sampled {
		j := window - sampled + i
		//nolint:gosec // sampled never exceeds window, so j stays non-negative and j+1 is a valid modulus.
		t := int64(binary.BigEndian.Uint64(rnd[8*i:]) % uint64(j+1))
		if _, ok := p.selectedSet[t]; ok {
			t = j
		}
		p.selectedSet[t] = struct{}{}
		allKeys[newKeys+i] = top - 1 - t
	}
	// Any references the window could not hold — and all of them when there is no window — step one key at
	// a time back from the top, the most-contended pattern.
	for i := sampled; i < nRefs; i++ {
		allKeys[newKeys+i] = top - 1 - i
	}
	return p.newTxSlotKeysFromArray(allKeys)
}

func (p *txRandomProcess) newTxSlotKeysFromArray(keys []int64) txSlotKeys {
	return txSlotKeys{
		readWrite:  keys[:p.ReadWriteCount],
		blindWrite: keys[p.ReadWriteCount : p.ReadWriteCount+p.BlindWriteCount],
		readOnly:   keys[p.ReadWriteCount+p.BlindWriteCount:],
	}
}

func fillNewKeys(newkeys []int64, frontier, count int64) {
	for i := range count {
		newkeys[i] = frontier + i
	}
}

// committedFrontier reports how many keys have been introduced before transaction txIdx. It grows by the
// effective new-key rate — the slot count minus the backref rate — each transaction, so a larger backref
// rate advances it more slowly and leaves more existing keys to reference. The rate may be fractional;
// rounding the accumulated count down is what turns a fractional rate into whole keys spread evenly across
// transactions instead of arriving in bursts. A zero backref rate advances the frontier by the full slot
// count every transaction (every slot fresh); a backref rate equal to the slot count freezes it at zero
// (nothing is ever created — a fully static working set).
func committedFrontier(txIdx uint64, newKeysRate float64) int64 {
	return int64(float64(txIdx) * newKeysRate)
}

// invalidSignature decides, deterministically from the transaction index, whether this transaction
// gets a bad signature — so the set of invalid transactions is worker-count invariant. The signature
// bytes stay non-deterministic and outside the contract; only the decision is reproducible.
func (p *txRandomProcess) invalidSignature(txIdx uint64) bool {
	if p.InvalidSignatures <= 0 {
		return false
	}
	if p.InvalidSignatures >= 1 {
		return true
	}
	u := p.deriveUint64(domainSign, txIdx)
	// 53-bit uniform in [0,1), matching math/rand/v2.Float64.
	return float64(u>>11)/float64(uint64(1)<<53) < p.InvalidSignatures
}

// deriveUint64 derives 8 bytes at (domain, index) and reads them as a big-endian uint64 — the integer
// form of the derivation primitive, used for the per-transaction reference offset and the invalid-
// signature decision.
func (p *txRandomProcess) deriveUint64(domain byte, index uint64) uint64 {
	return binary.BigEndian.Uint64(p.derive(domain, index, 0, 8))
}

// derive returns size bytes bound to (rootSeed, domain, index, subIndex), or nil when size is 0. It is
// a pure function of its inputs — the basis for O(1) index-addressable regeneration. It reuses the
// process's preallocated seed buffer (rootSeed prefix already set), rewriting only the domain byte and
// the two index fields; this is safe because a process is driven from a single goroutine. Bytes come
// from SHA-256 in MGF1-style counter mode (a single hash truncated to size for size <= 32; additional
// SHA-256(seed || counter) blocks otherwise), so it costs O(ceil(size/32)) hash blocks.
func (p *txRandomProcess) derive(domain byte, index, subIndex uint64, size uint32) []byte {
	if size == 0 {
		return nil
	}
	p.seed[domainOffset] = domain
	binary.BigEndian.PutUint64(p.seed[indexOffset:subIndexOffset], index)
	binary.BigEndian.PutUint64(p.seed[subIndexOffset:blockOffset], subIndex)

	out := make([]byte, 0, size+sha256.Size)
	for block := uint32(0); len(out) < int(size); block++ {
		binary.BigEndian.PutUint32(p.seed[blockOffset:], block)
		h := sha256.New()
		_, _ = h.Write(p.seed[:])
		out = h.Sum(out)
	}
	return out[:size]
}
