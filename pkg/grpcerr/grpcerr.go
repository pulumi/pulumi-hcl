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

// Package grpcerr maps the errors pulumi-hcl produces onto gRPC status codes,
// so callers like `pulumi package get-schema` can classify a failure by its
// code instead of parsing message text.
//
// The codes are the machine-readable contract: Unavailable and
// DeadlineExceeded are retriable, everything else is not. Where one code
// covers failures that a caller must handle differently, the status also
// carries a google.rpc.ErrorInfo detail with a Reason in [Domain].
package grpcerr

import (
	"context"
	"errors"
	"fmt"

	p "github.com/pulumi/pulumi-go-provider"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/contract"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/status"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/modules"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
)

// statusError attaches a gRPC status code to err without altering its message.
// grpc-go's status.FromError finds GRPCStatus through wrapped chains, so the
// code survives further fmt.Errorf("%w") wrapping on the way to the wire.
type statusError struct {
	code codes.Code
	// reason, when set, is attached as the Reason of an ErrorInfo detail.
	reason string
	err    error
}

// Domain is the ErrorInfo domain of every reason that pulumi-hcl reports.
const Domain = "hcl.pulumi.com"

// ErrorInfo reasons. Each one refines the gRPC code it is reported with.
const (
	// ReasonNoCredentials refines Unauthenticated: a module registry refused
	// a request that carried no credentials, because none are configured for
	// its host.
	ReasonNoCredentials = "NO_CREDENTIALS"
	// ReasonCredentialsRejected refines Unauthenticated: a module registry
	// rejected the credentials that the request carried.
	ReasonCredentialsRejected = "CREDENTIALS_REJECTED"
)

func (e statusError) Error() string { return e.err.Error() }
func (e statusError) Unwrap() error { return e.err }

func (e statusError) GRPCStatus() *status.Status {
	s := status.New(e.code, e.err.Error())
	if e.reason == "" {
		return s
	}
	s, err := s.WithDetails(&errdetails.ErrorInfo{Reason: e.reason, Domain: Domain})
	contract.AssertNoErrorf(err, "attaching ErrorInfo to a %v status", e.code)
	return s
}

// Errorf builds an error carrying the given gRPC status code.
func Errorf(code codes.Code, format string, args ...any) error {
	return statusError{code: code, err: fmt.Errorf(format, args...)}
}

// Classify assigns err the gRPC status code implied by its chain, falling
// back to fallback when nothing in the chain classifies it. An error already
// carrying a status — including one returned by a downstream RPC — keeps it.
func Classify(err error, fallback codes.Code) error {
	var grpcStatus interface{ GRPCStatus() *status.Status }
	switch {
	case err == nil:
		return nil
	case errors.As(err, &grpcStatus):
		return err
	case errors.Is(err, context.Canceled):
		return statusError{code: codes.Canceled, err: err}
	case errors.Is(err, context.DeadlineExceeded):
		return statusError{code: codes.DeadlineExceeded, err: err}
	case errors.Is(err, modules.ErrNotFound):
		return statusError{code: codes.NotFound, err: err}
	case errors.Is(err, modules.ErrUnauthenticated):
		return statusError{code: codes.Unauthenticated, reason: ReasonCredentialsRejected, err: err}
	case errors.Is(err, modules.ErrPermissionDenied):
		return statusError{code: codes.PermissionDenied, err: err}
	case errors.Is(err, modules.ErrNoCredentials):
		return statusError{code: codes.Unauthenticated, reason: ReasonNoCredentials, err: err}
	case errors.Is(err, modules.ErrTransient):
		return statusError{code: codes.Unavailable, err: err}
	case errors.Is(err, modules.ErrInvalid):
		return statusError{code: codes.InvalidArgument, err: err}
	case errors.Is(err, packages.ErrNotFound):
		// The module references a type the resolved provider schema does not
		// carry: converting this module is unsupported, not retriable.
		return statusError{code: codes.Unimplemented, err: err}
	default:
		return statusError{code: fallback, err: err}
	}
}

// Wrap is a middleware that classifies every error provider returns with
// [Classify] before it reaches the RPC layer. Methods provider does not
// implement stay unimplemented.
func Wrap(provider p.Provider) p.Provider {
	provider.Handshake = delegateIO(provider.Handshake)
	provider.GetSchema = delegateIO(provider.GetSchema)
	provider.Parameterize = delegateIO(provider.Parameterize)
	provider.Cancel = delegate(provider.Cancel)
	provider.CheckConfig = delegateIO(provider.CheckConfig)
	provider.DiffConfig = delegateIO(provider.DiffConfig)
	provider.Configure = delegateI(provider.Configure)
	provider.Invoke = delegateIO(provider.Invoke)
	provider.Check = delegateIO(provider.Check)
	provider.Diff = delegateIO(provider.Diff)
	provider.Create = delegateIO(provider.Create)
	provider.Read = delegateIO(provider.Read)
	provider.Update = delegateIO(provider.Update)
	provider.Delete = delegateI(provider.Delete)
	provider.Construct = delegateIO(provider.Construct)
	provider.Call = delegateIO(provider.Call)
	return provider
}

func delegateIO[I, O any, F func(context.Context, I) (O, error)](method F) F {
	if method == nil {
		return nil
	}
	return func(ctx context.Context, req I) (O, error) {
		resp, err := method(ctx, req)
		return resp, Classify(err, codes.Unknown)
	}
}

func delegateI[I any, F func(context.Context, I) error](method F) F {
	if method == nil {
		return nil
	}
	return func(ctx context.Context, req I) error {
		return Classify(method(ctx, req), codes.Unknown)
	}
}

func delegate[F func(context.Context) error](method F) F {
	if method == nil {
		return nil
	}
	return func(ctx context.Context) error {
		return Classify(method(ctx), codes.Unknown)
	}
}
