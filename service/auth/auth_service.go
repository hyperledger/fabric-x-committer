/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"bytes"
	"context"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/msppb"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/common/util"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
	"google.golang.org/grpc/health"
	healthgrpc "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/protobuf/types/known/timestamppb"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/channel"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/monitoring"
	"github.com/hyperledger/fabric-x-committer/utils/serve"
	"github.com/hyperledger/fabric-x-committer/utils/statedb"
)

var logger = flogging.MustGetLogger("auth")

// authEnvelopeType is the header type an authentication envelope carries.
const authEnvelopeType = int32(common.HeaderType_MESSAGE)

type (
	// Service is the authentication and authorization service.
	// It holds no per-connection state, so any instance can serve any client's request.
	Service struct {
		servicepb.UnimplementedAuthServiceServer

		config  *Config
		metrics *perfMetrics
		// challenges throttles the two RPCs reachable without a token. Nil when the limit is disabled.
		challenges  *rate.Limiter
		ready       *channel.Ready
		healthcheck *health.Server

		// db and configProvider are set by Run, which opens the database pool they read from.
		db             *database
		configProvider *configProvider
	}

	// parsedEnvelope holds the pieces verifyEnvelope inspects. payloadData is empty for a genuine
	// authentication envelope; nonce is what the client claims from the SignatureHeader.
	parsedEnvelope struct {
		chdr        *common.ChannelHeader
		payloadData []byte
		nonce       []byte
		signedData  *protoutil.SignedData
	}

	// verifiedIdentity is the outcome of authenticating an envelope. certHash is nil when the client
	// connected without a certificate.
	verifiedIdentity struct {
		identity *msppb.Identity
		certHash []byte
	}
)

var (
	// ErrStaleEnvelope is returned when an authentication envelope's timestamp is missing, invalid,
	// or outside the configured freshness window.
	ErrStaleEnvelope = errors.New("authentication envelope is stale")
	// ErrCertBindingMismatch is returned when the envelope's claimed TLS certificate hash does not
	// match the certificate presented on the connection.
	ErrCertBindingMismatch = errors.New("TLS certificate binding mismatch")
	// ErrEnvelopeScope is returned when an envelope is not scoped to authentication for this channel
	// (wrong header type or channel id).
	ErrEnvelopeScope = errors.New("envelope is not an authentication request for this channel")
	// ErrNoEnvelope is returned when the request carries no signed envelope at all.
	ErrNoEnvelope = errors.New("signed envelope is required")
)

// NewAuthService creates a new AuthService from a configuration.
func NewAuthService(config *Config) (*Service, error) {
	if err := config.ChallengeRateLimit.Validate(); err != nil {
		return nil, errors.Newf("invalid challenge rate limit: %v", err)
	}
	return &Service{
		config:      config,
		metrics:     newAuthServiceMetrics(),
		challenges:  serve.NewRateLimiter(&config.ChallengeRateLimit),
		ready:       channel.NewReady(),
		healthcheck: serve.DefaultHealthCheckService(),
	}, nil
}

// Run opens the database pool, starts the configuration refresh and the expired-record sweep, signals
// readiness, and blocks until the context is done.
func (s *Service) Run(ctx context.Context) error {
	logger.Infof("Starting auth service with token TTL: %s, and nonce TTL: %s",
		s.config.TokenTTL, s.config.NonceTTL)

	pool, err := statedb.NewPool(ctx, s.config.Database)
	if err != nil {
		return err
	}
	defer pool.Close()

	s.db = &database{
		pool:  pool,
		retry: s.config.Database.Retry,
	}
	s.configProvider = &configProvider{
		db:      s.db,
		metrics: s.metrics,
	}

	g, gCtx := errgroup.WithContext(ctx)
	g.Go(func() error {
		return s.configProvider.run(
			gCtx, s.config.ConfigRefreshInterval,
		)
	})
	g.Go(func() error {
		return s.sweepExpiredRecords(gCtx)
	})

	s.ready.SignalReady()
	defer s.ready.Reset()

	return g.Wait()
}

// WaitForReady waits until the service is ready to answer requests, or returns false if the context
// ended first.
func (s *Service) WaitForReady(ctx context.Context) bool {
	return s.ready.WaitForReady(ctx)
}

// RegisterService registers the AuthService's gRPC handlers and monitoring server.
func (s *Service) RegisterService(srv serve.Servers) {
	servicepb.RegisterAuthServiceServer(srv.GRPC, s)
	healthgrpc.RegisterHealthServer(srv.GRPC, s.healthcheck)
	monitoring.RegisterMonitoringServer(srv.HTTP, s.metrics.Provider)
	serve.RegisterServerMetrics(srv.StatsHandler, s.metrics.serverMetrics)
}

// IssueNonce issues a single-use nonce for the client's next Authenticate.
func (s *Service) IssueNonce(
	ctx context.Context, _ *servicepb.IssueNonceRequest,
) (*servicepb.IssueNonceResponse, error) {
	if err := s.allowChallenge(); err != nil {
		return nil, grpcerror.WrapResourceExhaustedOrCancelled(ctx, err)
	}
	expiresAt := time.Now().Add(s.config.NonceTTL)
	nonce, err := s.db.insertNonce(ctx, expiresAt)
	if err != nil {
		return nil, grpcerror.WrapInternalError(err)
	}
	return &servicepb.IssueNonceResponse{
		Nonce:     nonce,
		ExpiresAt: expiresAt.Unix(),
	}, nil
}

// Authenticate exchanges a signed envelope for a cert-bound token. The signature is verified once here;
// subsequent authorization carries the identity forward via the persisted token record.
func (s *Service) Authenticate(
	ctx context.Context, req *servicepb.AuthenticateRequest,
) (*servicepb.AuthenticateResponse, error) {
	if err := s.allowChallenge(); err != nil {
		return nil, grpcerror.WrapResourceExhaustedOrCancelled(ctx, err)
	}
	bundle, err := s.configProvider.current()
	if err != nil {
		return nil, grpcerror.WrapUnavailable(err)
	}
	if req.GetSignedEnvelope() == nil {
		return nil, grpcerror.WrapInvalidArgument(ErrNoEnvelope)
	}

	now := time.Now()
	parsed, err := parseSignedEnvelope(req.GetSignedEnvelope())
	if err != nil {
		return nil, grpcerror.WrapUnauthenticated(errors.Newf("authentication failed: %v", err))
	}

	// Redeem before verifying the signature: redeeming first stops a replayer from making
	// the service repeat the expensive signature check.
	if err = s.db.consumeNonce(ctx, parsed.nonce, now); err != nil {
		if !errors.Is(err, errNonceNotFound) {
			return nil, grpcerror.WrapInternalError(err)
		}
		return nil, grpcerror.WrapUnauthenticated(errors.Newf("authentication failed: %v", err))
	}

	identity, err := s.verifyEnvelope(ctx, parsed, bundle, now)
	if err != nil {
		return nil, grpcerror.WrapUnauthenticated(errors.Newf("authentication failed: %v", err))
	}

	rec := &servicepb.TokenRecord{
		Identity:       identity.identity,
		CertHashSha256: identity.certHash,
		Scope:          normalizeScope(req.GetRequestedScope()),
		ExpiresAt:      now.Add(s.config.TokenTTL).Unix(),
	}
	token, err := s.db.insertToken(ctx, rec)
	if err != nil {
		return nil, grpcerror.WrapInternalError(err)
	}

	return &servicepb.AuthenticateResponse{
		Token:     token,
		ExpiresAt: rec.GetExpiresAt(),
	}, nil
}

// Authorize resolves a token, checks its expiry, binding and scope, and evaluates the resource policy
// against its identity. A resource server calls it per RPC and, for a stream, whenever its cached decision
// lapses, so expiry and configuration changes both take effect.
func (s *Service) Authorize(
	ctx context.Context, req *servicepb.AuthorizeRequest,
) (*servicepb.AuthorizeResponse, error) {
	bundle, err := s.configProvider.current()
	if err != nil {
		return nil, grpcerror.WrapUnavailable(err)
	}

	tokenRecord, err := s.db.readToken(ctx, req.GetToken())
	switch {
	case errors.Is(err, ErrTokenNotFound):
		return nil, grpcerror.WrapUnauthenticated(errors.New("token is not recognized"))
	case err != nil:
		return nil, grpcerror.WrapUnavailable(errors.New("the token store is unavailable"))
	}

	if tokenRecord.GetExpiresAt() <= time.Now().Unix() {
		return nil, grpcerror.WrapUnauthenticated(errors.New("token has expired"))
	}

	// The certificate presented at the resource server must match the one the token was bound to, so
	// a leaked token cannot be replayed from a different connection.
	if !bytes.Equal(tokenRecord.GetCertHashSha256(), req.GetTlsCertHash()) {
		return nil, grpcerror.WrapUnauthenticated(errors.New("token is not bound to this certificate"))
	}

	// An empty scope imposes no restriction; a non-empty one allows only the exact methods it lists.
	if scope := tokenRecord.GetScope(); len(scope) > 0 && !slices.Contains(scope, req.GetResource()) {
		return nil, grpcerror.WrapPermissionDenied(
			errors.Newf("resource %s is outside the token scope", req.GetResource()),
		)
	}

	if err = evaluateResourcePolicy(bundle, req.GetResource(), tokenRecord.GetIdentity()); err != nil {
		logger.Debugf("Authorization denied for [%s]: %v", req.GetResource(), err)
		return nil, grpcerror.WrapPermissionDenied(err)
	}

	return &servicepb.AuthorizeResponse{
		TokenExpiresAt: tokenRecord.GetExpiresAt(),
	}, nil
}

// sweepExpiredRecords periodically deletes expired token and nonce records.
func (s *Service) sweepExpiredRecords(ctx context.Context) error {
	ticker := time.NewTicker(s.config.SweepInterval)
	defer ticker.Stop()

	for {
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
			deletedTokens, deletedNonces, err := s.db.deleteExpired(ctx, time.Now())
			if err != nil {
				logger.Errorf("Sweeping expired records failed: %v", err)
			} else if deletedTokens+deletedNonces > 0 {
				logger.Infof("Swept %d expired tokens and %d expired nonces", deletedTokens, deletedNonces)
			}
		}
	}
}

func (s *Service) allowChallenge() error {
	if s.challenges == nil || s.challenges.Allow() {
		return nil
	}
	return errors.New("rate limit exceeded")
}

// verifyEnvelope checks scoping, freshness, certificate binding, MSP resolution and the signature, then
// returns the identity. Redeeming the nonce stays in Authenticate: it is the one stateful step.
func (s *Service) verifyEnvelope(
	ctx context.Context, parsed *parsedEnvelope, bundle *channelconfig.Bundle, now time.Time,
) (*verifiedIdentity, error) {
	chdr := parsed.chdr
	if chdr.GetType() != authEnvelopeType {
		return nil, errors.Wrapf(ErrEnvelopeScope, "unexpected header type %d", chdr.GetType())
	}
	if expected := bundle.ConfigtxValidator().ChannelID(); chdr.GetChannelId() != expected {
		return nil, errors.Wrapf(ErrEnvelopeScope, "channel %q does not match %q", chdr.GetChannelId(), expected)
	}
	if len(parsed.payloadData) != 0 {
		return nil, errors.Wrapf(ErrEnvelopeScope,
			"authentication envelope must carry an empty payload, got %d bytes", len(parsed.payloadData))
	}

	if err := validateTimestamp(chdr.GetTimestamp(), s.config.EnvelopeFreshnessWindow, now); err != nil {
		return nil, err
	}

	// The token binds to the certificate on the connection, and the envelope must claim exactly that one:
	// a hash without a certificate, or a certificate without its hash, is refused rather than left unbound.
	certHash := util.ExtractCertificateHashFromContext(ctx)
	if !bytes.Equal(chdr.GetTlsCertHash(), certHash) {
		return nil, ErrCertBindingMismatch
	}

	signedData := parsed.signedData
	identity, err := bundle.MSPManager().DeserializeIdentity(signedData.Identity)
	if err != nil {
		return nil, errors.Wrap(err, "failed to deserialize identity")
	}
	if err = identity.Validate(); err != nil {
		return nil, errors.Wrap(err, "identity is not valid")
	}
	if err = identity.Verify(signedData.Data, signedData.Signature); err != nil {
		return nil, errors.Wrap(err, "signature verification failed")
	}

	return &verifiedIdentity{
		identity: signedData.Identity,
		certHash: certHash,
	}, nil
}

// parseSignedEnvelope unpacks the envelope into the pieces verifyEnvelope inspects. The nonce comes from
// the SignatureHeader, which the signature covers, so a replayer cannot swap in a fresh one.
func parseSignedEnvelope(env *common.Envelope) (*parsedEnvelope, error) {
	payload, err := protoutil.UnmarshalPayload(env.Payload)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal payload")
	}
	if payload.Header == nil {
		return nil, errors.New("envelope payload has no header")
	}
	chdr, err := protoutil.UnmarshalChannelHeader(payload.Header.ChannelHeader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal channel header")
	}
	shdr, err := protoutil.UnmarshalSignatureHeader(payload.Header.SignatureHeader)
	if err != nil {
		return nil, errors.Wrap(err, "failed to unmarshal signature header")
	}
	// if protoutil.EnvelopeAsSignedData didn't fail, the signedData is a slice of length 1.
	signedData, err := protoutil.EnvelopeAsSignedData(env)
	if err != nil {
		return nil, errors.Wrap(err, "failed to extract signed data from envelope")
	}
	return &parsedEnvelope{
		chdr:        chdr,
		payloadData: payload.Data,
		nonce:       shdr.GetNonce(),
		signedData:  signedData[0],
	}, nil
}

// validateTimestamp rejects a missing, unrepresentable or out-of-window timestamp.
func validateTimestamp(ts *timestamppb.Timestamp, window time.Duration, now time.Time) error {
	if err := ts.CheckValid(); err != nil {
		return errors.Wrapf(ErrStaleEnvelope, "invalid timestamp: %v", err)
	}

	t := ts.AsTime()
	if t.Before(now.Add(-window)) || t.After(now.Add(window)) {
		return errors.Wrapf(ErrStaleEnvelope,
			"timestamp %s is outside the freshness window of %s around %s", t, window, now)
	}
	return nil
}

// normalizeScope trims and de-duplicates a requested scope of gRPC full-method names.
func normalizeScope(requested []string) []string {
	normalized := make([]string, 0, len(requested))
	for _, entry := range requested {
		entry = strings.TrimSpace(entry)
		if entry == "" || slices.Contains(normalized, entry) {
			continue
		}
		normalized = append(normalized, entry)
	}
	if len(normalized) == 0 {
		return nil
	}
	return normalized
}
