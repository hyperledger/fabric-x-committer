/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

	"github.com/hyperledger/fabric-x-committer/api/servicepb"
	"github.com/hyperledger/fabric-x-committer/utils/acl"
	"github.com/hyperledger/fabric-x-committer/utils/connection"
	"github.com/hyperledger/fabric-x-committer/utils/grpcerror"
	"github.com/hyperledger/fabric-x-committer/utils/test"
)

// TestAuthSecureConnection verifies the auth service gRPC server's behavior
// under various client TLS configurations.
func TestAuthSecureConnection(t *testing.T) {
	t.Parallel()
	test.RunSecureConnectionTest(
		t,
		func(t *testing.T, serverTLS, clientTLS connection.TLSConfig) test.RPCAttempt {
			t.Helper()
			env := NewAuthTestEnv(t, &TestEnvParams{ServerTLS: serverTLS, ClientTLS: clientTLS})
			return func(ctx context.Context, t *testing.T, cfg connection.TLSConfig) error {
				t.Helper()
				client := test.CreateClientWithTLS(
					t, env.Config.Endpoints[0], cfg, servicepb.NewAuthServiceClient,
				)
				_, err := client.IssueNonce(ctx, nil)
				return err
			}
		},
	)
}

// TestAuthAuthorizeCases verifies the auth service's Authorize method under various token and resource conditions.
func TestAuthAuthorizeCases(t *testing.T) {
	t.Parallel()

	artifactsPath := t.TempDir()
	env := newAuthTestEnvWithMutualTLS(t, TestEnvParams{ArtifactsPath: artifactsPath})

	token := env.IssueToken(t)

	scopedParams := *env.IssueParams
	scopedParams.Scope = []string{committerpb.QueryService_GetRows_FullMethodName}
	resp, err := acl.IssueToken(t.Context(), &scopedParams)
	require.NoError(t, err)
	scopedToken := resp.GetToken()

	// A consenter belongs to the orderer organization: a channel member, so it authenticates, yet outside
	// the application organizations that the Readers policy admits.
	consenters, err := testcrypto.GetConsenterIdentities(artifactsPath)
	require.NoError(t, err)
	require.NotEmpty(t, consenters)
	consenterParams := *env.IssueParams
	consenterParams.Signer = consenters[0]
	resp, err = acl.IssueToken(t.Context(), &consenterParams)
	require.NoError(t, err)
	consenterToken := resp.GetToken()

	// Success cases.
	for _, tc := range []struct {
		name  string
		token string
	}{
		{name: "unscoped token", token: token},
		{name: "scoped token within its scope", token: scopedToken},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := env.Client.Authorize(t.Context(), &servicepb.AuthorizeRequest{
				Token:       tc.token,
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			})
			require.NoError(t, err)
			// The resource server bounds its cached decision for a stream by the token's own expiry.
			require.Greater(t, resp.GetTokenExpiresAt(), time.Now().Unix())
		})
	}

	// Failure cases.
	for _, tc := range []struct {
		name     string
		request  *servicepb.AuthorizeRequest
		wantCode codes.Code
	}{
		{
			name: "unknown token",
			request: &servicepb.AuthorizeRequest{
				Token:       "never-issued",
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name: "empty token",
			request: &servicepb.AuthorizeRequest{
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name: "certificate mismatch",
			request: &servicepb.AuthorizeRequest{
				Token:       token,
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: []byte{0xFF},
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name: "no policy for resource",
			request: &servicepb.AuthorizeRequest{
				Token:       token,
				Resource:    "/unknown.Service/Method",
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			name: "identity outside the resource policy",
			request: &servicepb.AuthorizeRequest{
				Token:       consenterToken,
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			// Denied although the identity's policy would otherwise allow it.
			name: "scoped token outside its scope",
			request: &servicepb.AuthorizeRequest{
				Token:       scopedToken,
				Resource:    committerpb.QueryService_GetTransactionStatus_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.PermissionDenied,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := env.Client.Authorize(t.Context(), tc.request)
			require.Equal(t, tc.wantCode, grpcerror.GetCode(err))
		})
	}
}

// TestAuthorizeRejectsExpiredToken verifies that the auth service rejects tokens that have expired,
// even if they were valid when issued.
func TestAuthorizeRejectsExpiredToken(t *testing.T) {
	t.Parallel()
	env := newAuthTestEnvWithMutualTLS(t, TestEnvParams{TokenTTL: time.Second})
	token := env.IssueToken(t)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		_, err := env.Client.Authorize(t.Context(), &servicepb.AuthorizeRequest{
			Token: token, Resource: committerpb.QueryService_GetRows_FullMethodName, TlsCertHash: env.TLSCertHash,
		})
		require.Equal(ct, codes.Unauthenticated, grpcerror.GetCode(err))
		require.ErrorContains(ct, err, "token has expired")
	}, 10*time.Second, 200*time.Millisecond)
}

// TestAuthAuthenticateCases verifies the auth service's Authenticate method under various envelope conditions.
func TestAuthAuthenticateCases(t *testing.T) {
	t.Parallel()

	env := newAuthTestEnvWithMutualTLS(t, TestEnvParams{})

	// Success cases. Each envelope is signed with a fresh nonce inside its subtest: a genuine one reaches the
	// freshness checks, and a parallel subtest may start long after the table is built.
	for _, tc := range []struct {
		name  string
		scope []string
	}{
		{
			name: "unscoped token",
		},
		{
			name:  "token scoped to the requested methods",
			scope: []string{committerpb.QueryService_GetRows_FullMethodName},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			nonce, err := env.Client.IssueNonce(t.Context(), &servicepb.IssueNonceRequest{})
			require.NoError(t, err)
			resp, err := env.Client.Authenticate(t.Context(), &servicepb.AuthenticateRequest{
				SignedEnvelope: buildEnvelope(t, &env.EnvelopeParams, nonce.GetNonce()),
				RequestedScope: tc.scope,
			})
			require.NoError(t, err)
			require.NotEmpty(t, resp.GetToken())
			require.Greater(t, resp.GetExpiresAt(), time.Now().Unix())
		})
	}

	// Failure cases.
	// The nonce is single-use: once redeemed, the very same envelope is worthless to whoever captured it,
	// even inside the freshness window.
	resp, err := env.Client.IssueNonce(t.Context(), &servicepb.IssueNonceRequest{})
	require.NoError(t, err)
	replayed := buildEnvelope(t, &env.EnvelopeParams, resp.GetNonce())
	_, err = env.Client.Authenticate(t.Context(), &servicepb.AuthenticateRequest{SignedEnvelope: replayed})
	require.NoError(t, err)

	// Rejected before the envelope is verified, so these may be built up front: neither their timestamp
	// nor their nonce is ever checked for freshness.
	for _, tc := range []struct {
		name     string
		envelope *common.Envelope
		wantCode codes.Code
		wantErr  string
	}{
		{
			name:     "absent envelope",
			wantCode: codes.InvalidArgument,
			wantErr:  ErrNoEnvelope.Error(),
		},
		{
			name:     "payload has no header",
			envelope: &common.Envelope{},
			wantCode: codes.Unauthenticated,
			wantErr:  "envelope payload has no header",
		},
		{
			// A client cannot invent its own challenge: only a nonce the server issued is redeemable.
			name:     "nonce the server never issued",
			envelope: buildEnvelope(t, &env.EnvelopeParams, []byte("home-made-nonce")),
			wantCode: codes.Unauthenticated,
			wantErr:  errNonceNotFound.Error(),
		},
		{
			name:     "no nonce at all",
			envelope: buildEnvelope(t, &env.EnvelopeParams, nil),
			wantCode: codes.Unauthenticated,
			wantErr:  "envelope carries no nonce",
		},
		{
			name:     "replayed envelope",
			envelope: replayed,
			wantCode: codes.Unauthenticated,
			wantErr:  errNonceNotFound.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, authErr := env.Client.Authenticate(t.Context(), &servicepb.AuthenticateRequest{
				SignedEnvelope: tc.envelope,
			})
			require.Equal(t, tc.wantCode, grpcerror.GetCode(authErr))
			require.ErrorContains(t, authErr, tc.wantErr)
		})
	}
}

// newAuthTestEnvWithMutualTLS starts an AuthService whose tokens are bound to the client's certificate.
func newAuthTestEnvWithMutualTLS(t *testing.T, params TestEnvParams) *TestEnv {
	t.Helper()
	credentials := test.NewCredentialsFactory(t)
	params.ServerTLS, _ = credentials.CreateServerCredentials(t, connection.MutualTLSMode, "127.0.0.1")
	params.ClientTLS, _ = credentials.CreateClientCredentials(t, connection.MutualTLSMode)
	return NewAuthTestEnv(t, &params)
}

// buildEnvelope builds a genuine authentication envelope, exactly as a client does.
func buildEnvelope(t *testing.T, params *acl.EnvelopeParams, nonce []byte) *common.Envelope {
	t.Helper()
	envelope, err := acl.BuildAuthEnvelope(params, nonce)
	require.NoError(t, err)
	return envelope
}
