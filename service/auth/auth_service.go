/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"bytes"
	"context"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/cockroachdb/errors"
	"github.com/hyperledger/fabric-lib-go/common/flogging"
	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-protos-go-apiv2/peer"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/api/msppb"
	"github.com/hyperledger/fabric-x-common/common/channelconfig"
	"github.com/hyperledger/fabric-x-common/common/util"
	"github.com/hyperledger/fabric-x-common/protoutil"
	"golang.org/x/sync/errgroup"
	"golang.org/x/time/rate"
	"google.golang.org/grpc"
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
		committerpb.UnimplementedAuthServiceServer

		config  *Config
		metrics *perfMetrics
		// challenges throttles the two RPCs reachable without a token. Nil when the limit is disabled.
		challenges  *rate.Limiter
		ready       *channel.Ready
		healthcheck *health.Server
		// protectedRPCs are the gRPC full-method names a token's scope may name.
		protectedRPCs map[string]bool

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
)

var (
	// errStaleEnvelope is returned when an authentication envelope's timestamp is missing, invalid,
	// or outside the configured freshness window.
	errStaleEnvelope = errors.New("authentication envelope is stale")
	// errCertBindingMismatch is returned when the envelope's claimed TLS certificate hash does not
	// match the certificate presented on the connection.
	errCertBindingMismatch = errors.New("TLS certificate binding mismatch")
	// errEnvelopeScope is returned when an envelope is not scoped to authentication for this channel
	// (wrong header type or channel id).
	errEnvelopeScope = errors.New("envelope is not an authentication request for this channel")
	// errNoEnvelope is returned when the request carries no signed envelope at all.
	errNoEnvelope = errors.New("signed envelope is required")
	// errUnknownScope is returned when a requested scope names an RPC the resource servers do not protect.
	errUnknownScope = errors.New("requested scope names an unknown RPC")
)

// NewAuthService creates a new AuthService from a configuration.
func NewAuthService(config *Config) (*Service, error) {
	if err := config.ChallengeRateLimit.Validate(); err != nil {
		return nil, fmt.Errorf("invalid challenge rate limit: %w", err)
	}

	protected := make(map[string]bool)
	for _, desc := range []*grpc.ServiceDesc{
		&committerpb.QueryService_ServiceDesc, &committerpb.SidecarService_ServiceDesc, &peer.Deliver_ServiceDesc,
	} {
		for _, m := range desc.Methods {
			protected[fmt.Sprintf("/%s/%s", desc.ServiceName, m.MethodName)] = true
		}
		for _, st := range desc.Streams {
			protected[fmt.Sprintf("/%s/%s", desc.ServiceName, st.StreamName)] = true
		}
	}

	return &Service{
		config:        config,
		metrics:       newAuthServiceMetrics(),
		challenges:    serve.NewRateLimiter(&config.ChallengeRateLimit),
		ready:         channel.NewReady(),
		healthcheck:   serve.DefaultHealthCheckService(),
		protectedRPCs: protected,
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
	committerpb.RegisterAuthServiceServer(srv.GRPC, s)
	healthgrpc.RegisterHealthServer(srv.GRPC, s.healthcheck)
	monitoring.RegisterMonitoringServer(srv.HTTP, s.metrics.Provider)
	serve.RegisterServerMetrics(srv.StatsHandler, s.metrics.serverMetrics)
}

// IssueNonce issues a single-use nonce for the client's next Authenticate.
func (s *Service) IssueNonce(
	ctx context.Context, _ *committerpb.IssueNonceRequest,
) (*committerpb.IssueNonceResponse, error) {
	if err := s.allowChallenge(); err != nil {
		return nil, grpcerror.WrapResourceExhaustedOrCancelled(ctx, err)
	}
	expiresAt := time.Now().Add(s.config.NonceTTL)
	nonce, err := s.db.insertNonce(ctx, expiresAt)
	if err != nil {
		logger.Errorf("Failed to persist nonce: %+v", err)
		return nil, grpcerror.WrapInternalError(errors.New("failed to issue a nonce"))
	}
	return &committerpb.IssueNonceResponse{
		Nonce:     nonce,
		ExpiresAt: expiresAt.Unix(),
	}, nil
}

// Authenticate exchanges a signed envelope for a cert-bound token. The signature is verified once here;
// subsequent authorization carries the identity forward via the persisted token record.
func (s *Service) Authenticate(
	ctx context.Context, req *committerpb.AuthenticateRequest,
) (*committerpb.AuthenticateResponse, error) {
	if err := s.allowChallenge(); err != nil {
		return nil, grpcerror.WrapResourceExhaustedOrCancelled(ctx, err)
	}
	bundle, err := s.configProvider.current()
	if err != nil {
		return nil, grpcerror.WrapUnavailable(err)
	}
	if req.GetSignedEnvelope() == nil {
		return nil, grpcerror.WrapInvalidArgument(errNoEnvelope)
	}
	scope, err := normalizeScope(req.GetRequestedScope(), s.protectedRPCs)
	if err != nil {
		return nil, grpcerror.WrapInvalidArgument(err)
	}

	now := time.Now()
	parsed, err := parseSignedEnvelope(req.GetSignedEnvelope())
	if err != nil {
		return nil, grpcerror.WrapUnauthenticated(fmt.Errorf("authentication failed: %w", err))
	}

	// Redeem before verifying the signature: redeeming first stops a replayer from making
	// the service repeat the expensive signature check.
	if err = s.db.consumeNonce(ctx, parsed.nonce, now); err != nil {
		if !errors.Is(err, errNonceNotFound) {
			logger.Errorf("Failed to redeem nonce: %+v", err)
			return nil, grpcerror.WrapInternalError(errors.New("failed to redeem the nonce"))
		}
		return nil, grpcerror.WrapUnauthenticated(fmt.Errorf("authentication failed: %w", err))
	}

	certHash := util.ExtractCertificateHashFromContext(ctx)
	identity, err := s.verifyEnvelope(parsed, bundle, certHash, now)
	if err != nil {
		logger.Debugf("Envelope verification failed: %v", err)
		return nil, grpcerror.WrapUnauthenticated(errors.New("authentication failed"))
	}

	rec := &servicepb.TokenRecord{
		Identity:       identity,
		CertHashSha256: certHash,
		Scope:          scope,
		ExpiresAt:      now.Add(s.config.TokenTTL).Unix(),
	}
	token, err := s.db.insertToken(ctx, rec)
	if err != nil {
		logger.Errorf("Failed to persist token record: %+v", err)
		return nil, grpcerror.WrapInternalError(errors.New("failed to issue a token"))
	}

	return &committerpb.AuthenticateResponse{
		Token:     token,
		ExpiresAt: rec.GetExpiresAt(),
	}, nil
}

// Authorize resolves a token, checks its expiry, binding and scope, and evaluates the resource policy
// against its identity. A resource server calls it per RPC and, for a stream, whenever its cached decision
// lapses, so expiry and configuration changes both take effect.
func (s *Service) Authorize(
	ctx context.Context, req *committerpb.AuthorizeRequest,
) (*committerpb.AuthorizeResponse, error) {
	bundle, err := s.configProvider.current()
	if err != nil {
		return nil, grpcerror.WrapUnavailable(err)
	}

	tokenRecord, err := s.db.readToken(ctx, req.GetToken())
	switch {
	case errors.Is(err, errTokenNotFound):
		return nil, grpcerror.WrapUnauthenticated(fmt.Errorf("authorization failed: %w", err))
	case err != nil:
		logger.Errorf("Failed to read token record: %+v", err)
		return nil, grpcerror.WrapInternalError(errors.New("failed to read the token record"))
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

	return &committerpb.AuthorizeResponse{
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
	parsed *parsedEnvelope, bundle *channelconfig.Bundle, certHash []byte, now time.Time,
) (*msppb.Identity, error) {
	chdr := parsed.chdr
	if chdr.GetType() != authEnvelopeType {
		return nil, errors.Wrapf(errEnvelopeScope, "unexpected header type %d", chdr.GetType())
	}
	if expected := bundle.ConfigtxValidator().ChannelID(); chdr.GetChannelId() != expected {
		return nil, errors.Wrapf(errEnvelopeScope, "channel %q does not match %q", chdr.GetChannelId(), expected)
	}
	if len(parsed.payloadData) != 0 {
		return nil, errors.Wrapf(errEnvelopeScope,
			"authentication envelope must carry an empty payload, got %d bytes", len(parsed.payloadData))
	}

	if err := validateTimestamp(chdr.GetTimestamp(), s.config.EnvelopeFreshnessWindow, now); err != nil {
		return nil, err
	}

	// The token binds to the certificate on the connection, and the envelope must claim exactly that one:
	// a hash without a certificate, or a certificate without its hash, is refused rather than left unbound.
	if !bytes.Equal(chdr.GetTlsCertHash(), certHash) {
		return nil, errCertBindingMismatch
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

	return signedData.Identity, nil
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
		return errors.Wrap(errors.Join(errStaleEnvelope, err), "invalid timestamp")
	}

	t := ts.AsTime()
	if t.Before(now.Add(-window)) || t.After(now.Add(window)) {
		return errors.Wrapf(errStaleEnvelope,
			"timestamp %s is outside the freshness window of %s around %s", t, window, now)
	}
	return nil
}

// normalizeScope trims and de-duplicates a requested scope of gRPC full-method names, refusing any entry that
// is not a protected RPC.
func normalizeScope(requested []string, protected map[string]bool) ([]string, error) {
	var normalized []string
	for _, entry := range requested {
		entry = strings.TrimSpace(entry)
		if entry == "" || slices.Contains(normalized, entry) {
			continue
		}
		if !protected[entry] {
			return nil, errUnknownScope
		}
		normalized = append(normalized, entry)
	}
	return normalized, nil
}
