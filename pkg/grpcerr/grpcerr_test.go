// Copyright 2026, Pulumi Corporation.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package grpcerr

import (
	"context"
	"errors"
	"fmt"
	"testing"

	"github.com/google/go-cmp/cmp"
	"github.com/google/go-cmp/cmp/cmpopts"
	p "github.com/pulumi/pulumi-go-provider"
	"github.com/stretchr/testify/require"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/testing/protocmp"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
)

func TestClassify(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name     string
		err      error
		fallback codes.Code
		want     codes.Code
		// reason is the expected ErrorInfo reason; empty means no details.
		reason string
	}{
		{
			name:     "module not found",
			err:      fmt.Errorf("loading module: %w", modules.ErrNotFound),
			fallback: codes.Unknown,
			want:     codes.NotFound,
		},
		{
			name:     "registry unauthenticated",
			err:      fmt.Errorf("loading module: %w", modules.ErrUnauthenticated),
			fallback: codes.Unknown,
			want:     codes.Unauthenticated,
			reason:   ReasonCredentialsRejected,
		},
		{
			name:     "registry permission denied",
			err:      fmt.Errorf("loading module: %w", modules.ErrPermissionDenied),
			fallback: codes.Unknown,
			want:     codes.PermissionDenied,
		},
		{
			name:     "no credentials for registry",
			err:      fmt.Errorf("loading module: %w", modules.ErrNoCredentials),
			fallback: codes.Unknown,
			want:     codes.Unauthenticated,
			reason:   ReasonNoCredentials,
		},
		{
			name:     "transient registry failure",
			err:      fmt.Errorf("loading module: %w", modules.ErrTransient),
			fallback: codes.Unknown,
			want:     codes.Unavailable,
		},
		{
			name:     "invalid source",
			err:      fmt.Errorf("loading module: %w", modules.ErrInvalid),
			fallback: codes.Unknown,
			want:     codes.InvalidArgument,
		},
		{
			name:     "unresolvable provider type",
			err:      fmt.Errorf("resolving resource: %w", &packages.NotFoundError{Token: "aws_foo"}),
			fallback: codes.Unknown,
			want:     codes.Unimplemented,
		},
		{
			name:     "context canceled",
			err:      fmt.Errorf("loading module: %w", context.Canceled),
			fallback: codes.Unknown,
			want:     codes.Canceled,
		},
		{
			name:     "context deadline exceeded",
			err:      fmt.Errorf("loading module: %w", context.DeadlineExceeded),
			fallback: codes.Unknown,
			want:     codes.DeadlineExceeded,
		},
		{
			name:     "existing status wins over fallback",
			err:      fmt.Errorf("generating schema: %w", Errorf(codes.InvalidArgument, "bad args")),
			fallback: codes.Unimplemented,
			want:     codes.InvalidArgument,
		},
		{
			name:     "downstream rpc status kept",
			err:      fmt.Errorf("loading schema: %w", status.Error(codes.Unavailable, "engine gone")),
			fallback: codes.Unknown,
			want:     codes.Unavailable,
		},
		{
			name:     "unclassified uses fallback",
			err:      errors.New("boom"),
			fallback: codes.Unimplemented,
			want:     codes.Unimplemented,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			err := Classify(tc.err, tc.fallback)
			require.Equal(t, tc.want, status.Code(err))
			require.Equal(t, tc.err.Error(), err.Error(), "classification must not alter the message")
			require.Empty(t, cmp.Diff(errorInfo(tc.reason), status.Convert(err).Details(),
				protocmp.Transform(), cmpopts.EquateEmpty()))
		})
	}

	require.NoError(t, Classify(nil, codes.Unknown))
}

// TestStatusSurvivesWrapping guards the wire contract: grpc-go's
// status.FromError must recover the code from a statusError buried in a
// fmt.Errorf chain, and rebuild the message from the outermost error.
func TestStatusSurvivesWrapping(t *testing.T) {
	t.Parallel()
	err := fmt.Errorf("generating schema: %w", Errorf(codes.Unimplemented, "cannot type local"))
	s, ok := status.FromError(err)
	require.True(t, ok)
	require.Equal(t, codes.Unimplemented, s.Code())
	require.Equal(t, "generating schema: cannot type local", s.Message())

	err = fmt.Errorf("resolving module source: %w", Classify(modules.ErrNoCredentials, codes.Unknown))
	s, ok = status.FromError(err)
	require.True(t, ok)
	require.Empty(t, cmp.Diff(errorInfo(ReasonNoCredentials), s.Details(), protocmp.Transform()))
}

// errorInfo returns the status details that a reason produces.
func errorInfo(reason string) []any {
	if reason == "" {
		return nil
	}
	return []any{&errdetails.ErrorInfo{Reason: reason, Domain: Domain}}
}

func TestWrap(t *testing.T) {
	t.Parallel()
	provider := Wrap(p.Provider{
		Parameterize: func(context.Context, p.ParameterizeRequest) (p.ParameterizeResponse, error) {
			return p.ParameterizeResponse{}, fmt.Errorf("loading module: %w", modules.ErrNotFound)
		},
		Configure: func(context.Context, p.ConfigureRequest) error {
			return fmt.Errorf("dialing: %w", modules.ErrTransient)
		},
		Cancel: func(context.Context) error { return nil },
	})

	_, err := provider.Parameterize(t.Context(), p.ParameterizeRequest{})
	require.Equal(t, codes.NotFound, status.Code(err))

	err = provider.Configure(t.Context(), p.ConfigureRequest{})
	require.Equal(t, codes.Unavailable, status.Code(err))

	require.NoError(t, provider.Cancel(t.Context()))
	require.Nil(t, provider.GetSchema, "unimplemented methods must stay unimplemented")
}
