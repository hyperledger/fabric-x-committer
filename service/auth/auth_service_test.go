/*
Copyright IBM Corp. All Rights Reserved.

SPDX-License-Identifier: Apache-2.0
*/

package auth

import (
	"bytes"
	"context"
	"testing"
	"time"

	"github.com/hyperledger/fabric-protos-go-apiv2/common"
	"github.com/hyperledger/fabric-x-common/api/committerpb"
	"github.com/hyperledger/fabric-x-common/utils/testcrypto"
	"github.com/stretchr/testify/assert"
	"github.com/stretchr/testify/require"
	"google.golang.org/grpc/codes"

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
					t, env.Config.Endpoints[0], cfg, committerpb.NewAuthServiceClient,
				)
				_, err := client.IssueNonce(ctx, nil)
				return err
			}
		},
	)
}

// TestAuthAuthorizeIdentityCases verifies the auth service's AuthorizeIdentity method under various token and
// resource conditions.
func TestAuthAuthorizeIdentityCases(t *testing.T) {
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
		name     string
		token    string
		resource string
	}{
		{name: "unscoped token", token: token, resource: committerpb.QueryService_GetRows_FullMethodName},
		{
			name: "scoped token within its scope", token: scopedToken,
			resource: committerpb.QueryService_GetRows_FullMethodName,
		},
		{
			// A resource the channel configuration's ACLs do not name falls back to the Readers policy.
			name: "resource with no ACL falls back to the Readers policy", token: token,
			resource: "/unknown.Service/Method",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			resp, err := env.Client.AuthorizeIdentity(t.Context(), &committerpb.AuthorizeIdentityRequest{
				Token:       tc.token,
				Resource:    tc.resource,
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
		request  *committerpb.AuthorizeIdentityRequest
		wantCode codes.Code
	}{
		{
			name: "unknown token",
			request: &committerpb.AuthorizeIdentityRequest{
				Token:       "never-issued",
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name: "empty token",
			request: &committerpb.AuthorizeIdentityRequest{
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name: "certificate mismatch",
			request: &committerpb.AuthorizeIdentityRequest{
				Token:       token,
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: []byte{0xFF},
			},
			wantCode: codes.Unauthenticated,
		},
		{
			name: "identity outside the resource policy",
			request: &committerpb.AuthorizeIdentityRequest{
				Token:       consenterToken,
				Resource:    committerpb.QueryService_GetRows_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.PermissionDenied,
		},
		{
			// Denied although the identity's policy would otherwise allow it.
			name: "scoped token outside its scope",
			request: &committerpb.AuthorizeIdentityRequest{
				Token:       scopedToken,
				Resource:    committerpb.QueryService_GetTransactionStatus_FullMethodName,
				TlsCertHash: env.TLSCertHash,
			},
			wantCode: codes.PermissionDenied,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, err := env.Client.AuthorizeIdentity(t.Context(), tc.request)
			require.Equal(t, tc.wantCode, grpcerror.GetCode(err))
		})
	}
}

// TestAuthorizeIdentityRejectsExpiredToken verifies that the auth service rejects tokens that have expired,
// even if they were valid when issued.
func TestAuthorizeIdentityRejectsExpiredToken(t *testing.T) {
	t.Parallel()
	env := newAuthTestEnvWithMutualTLS(t, TestEnvParams{TokenTTL: time.Second})
	token := env.IssueToken(t)

	require.EventuallyWithT(t, func(ct *assert.CollectT) {
		_, err := env.Client.AuthorizeIdentity(t.Context(), &committerpb.AuthorizeIdentityRequest{
			Token: token, Resource: committerpb.QueryService_GetRows_FullMethodName, TlsCertHash: env.TLSCertHash,
		})
		require.Equal(ct, codes.Unauthenticated, grpcerror.GetCode(err))
		require.ErrorContains(ct, err, "token has expired")
	}, 10*time.Second, 200*time.Millisecond)
}

// TestAuthAuthenticateIdentityCases verifies the auth service's AuthenticateIdentity method under various envelope
// conditions.
func TestAuthAuthenticateIdentityCases(t *testing.T) {
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
			nonce, err := env.Client.IssueNonce(t.Context(), &committerpb.IssueNonceRequest{})
			require.NoError(t, err)
			resp, err := env.Client.AuthenticateIdentity(t.Context(), &committerpb.AuthenticateIdentityRequest{
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
	resp, err := env.Client.IssueNonce(t.Context(), &committerpb.IssueNonceRequest{})
	require.NoError(t, err)
	replayed := buildEnvelope(t, &env.EnvelopeParams, resp.GetNonce())
	_, err = env.Client.AuthenticateIdentity(
		t.Context(), &committerpb.AuthenticateIdentityRequest{SignedEnvelope: replayed},
	)
	require.NoError(t, err)

	// withCertHash signs a fresh nonce with a certificate hash by desire.
	withCertHash := func(certHash []byte) *common.Envelope {
		resp, err := env.Client.IssueNonce(t.Context(), &committerpb.IssueNonceRequest{})
		require.NoError(t, err)
		params := env.EnvelopeParams
		params.TLSCertHash = certHash
		return buildEnvelope(t, &params, resp.GetNonce())
	}

	for _, tc := range []struct {
		name     string
		envelope *common.Envelope
		scope    []string
		wantCode codes.Code
		wantErr  string
	}{
		{
			name:     "absent envelope",
			wantCode: codes.InvalidArgument,
			wantErr:  errNoEnvelope.Error(),
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
			envelope: buildEnvelope(t, &env.EnvelopeParams, bytes.Repeat([]byte{0x01}, randomLength)),
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
		{
			name:     "certificate hash of another connection",
			envelope: withCertHash([]byte{0xFF}),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "no certificate hash on a mutual TLS connection",
			envelope: withCertHash(nil),
			wantCode: codes.Unauthenticated,
		},
		{
			name:     "scope naming an unknown RPC",
			envelope: withCertHash(env.TLSCertHash),
			scope:    []string{"/committerpb.QueryService/NoSuchMethod"},
			wantCode: codes.InvalidArgument,
			wantErr:  errUnknownScope.Error(),
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			_, authErr := env.Client.AuthenticateIdentity(t.Context(), &committerpb.AuthenticateIdentityRequest{
				SignedEnvelope: tc.envelope,
				RequestedScope: tc.scope,
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
