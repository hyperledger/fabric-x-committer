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
	// selectedSet is the reused membership set for Floyd's algorithm, which draws k distinct offsets out
	// of the W-key working set — a uniform sample WITHOUT replacement — for a transaction's backward
	// references. It is cleared and refilled per transaction to avoid a per-transaction allocation; this
	// is safe because the process is driven from a single goroutine.
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

// slotKeys computes the flat key index of every slot of transaction txIdx. Reads and writes share ONE
// key space — the committable keys created by write slots. The committable frontier C = C(txIdx) is the
// number of keys created before this transaction. With the split disabled (NewKeysRate == nil) every
// slot gets a globally-unique fresh index (freshSlotKeys, the historical workload); otherwise
// splitSlotKeys applies the frontier + working-set model. Indices are signed int64 (negative =
// pre-genesis warmup key).
func (p *txRandomProcess) slotKeys(txIdx uint64) txSlotKeys {
	if p.NewKeysRate == nil {
		return p.freshSlotKeys(txIdx)
	}
	return p.splitSlotKeys(txIdx)
}

// freshSlotKeys is the split-disabled layout: every slot across the whole run gets a distinct index, so
// no key is ever reused — contention-free workload.
func (p *txRandomProcess) freshSlotKeys(txIdx uint64) txSlotKeys {
	slotsPerTx := int64(p.ReadWriteCount + p.BlindWriteCount + p.ReadOnlyCount)
	committedFrontier := int64(txIdx) * slotsPerTx //nolint:gosec // global tx index fits int64 in practice.
	newkeys := make([]int64, slotsPerTx)
	fillNewKeys(newkeys, committedFrontier, slotsPerTx)
	return p.newTxSlotKeysFromArray(newkeys)
}

// splitSlotKeys is the split-enabled layout. Let C = C(txIdx) and newKeys = C(txIdx+1) - C:
//   - the first newKeys slots, in layout order (read-write, then blind-write, then read-only), get fresh
//     frontier indices at [C, C+newKeys): a new WRITE slot CREATES its key, while a new READ slot is a
//     NONEXISTENT READ — it never creates, so the frontier index it consumes is wasted (never becomes a
//     committed key). Only writes advance real committed state, but the frontier counts every introduced
//     index, write or read.
//   - every remaining slot BACKWARD-references an existing key drawn from the moving working set
//     [top-W, top), where top = C(max(0, txIdx-gap)) is the frontier `gap` transactions behind and
//     W = KeyLookbackWindow. The offsets are a UNIFORM SAMPLE WITHOUT REPLACEMENT of [0, W) drawn by
//     Floyd's
//     algorithm (seeded from the tx index), so a transaction's slots reference a distinct, scattered
//     subset of the window rather than a single contiguous run. Floyd's fixes the subset uniformly but
//     does not shuffle it, so which drawn key lands on which slot is not itself uniform — immaterial
//     here, where contention depends only on which keys are touched and that they are distinct.
//
// Fresh indices (>= C >= top) and backward references (< top) are disjoint, and sampling without
// replacement keeps the references distinct (Validate guarantees W >= total slots, so the window is
// always wide enough to draw one distinct key per slot). Backward indices go NEGATIVE during run-start
// warmup, and stay negative for the whole run when the rate is 0 (no creates); these are distinct
// pre-genesis keys no create ever produces, so we do not clamp.
func (p *txRandomProcess) splitSlotKeys(txIdx uint64) txSlotKeys {
	slotsPerTx := int64(p.ReadWriteCount + p.BlindWriteCount + p.ReadOnlyCount)
	rate := *p.NewKeysRate
	frontier := committedFrontier(txIdx, rate)
	newKeys := min(slotsPerTx, committedFrontier(txIdx+1, rate)-frontier) // in [0, slotsPerTx]
	allkeys := make([]int64, slotsPerTx)
	fillNewKeys(allkeys, frontier, newKeys)

	window := int64(p.KeyLookbackWindow) //nolint:gosec // key-lookback-window config value fits int64.
	top := committedFrontier(txIdx-min(txIdx, p.TxReferenceGap), rate)
	nRefs := slotsPerTx - newKeys
	rnd := p.derive(domainRef, txIdx, 0, uint32(8*nRefs)) //nolint:gosec // small slot count fits uint32.
	clear(p.selectedSet)
	for i := range slotsPerTx - newKeys {
		j := window - nRefs + i
		//nolint:gosec // j+1 >= 1 since Validate guarantees window >= nRefs, so the modulus is positive.
		t := int64(binary.BigEndian.Uint64(rnd[8*i:]) % uint64(j+1))
		if _, ok := p.selectedSet[t]; ok {
			t = j
		}
		p.selectedSet[t] = struct{}{}
		allkeys[newKeys+i] = top - 1 - t
	}
	return p.newTxSlotKeysFromArray(allkeys)
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

// committedFrontier is the number of committable keys created (by write slots) before transaction txIdx:
// C(txIdx) = floor(txIdx * rate), where rate is the dereferenced NewKeysRate. It is only meaningful with
// the split enabled (rate from a non-nil NewKeysRate); a rate of 0 yields a constant frontier of 0 (no
// creates — the static working-set regime).
func committedFrontier(txIdx uint64, rate float64) int64 {
	return int64(float64(txIdx) * rate)
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
