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

package server

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"

	"github.com/google/uuid"
	"github.com/pulumi/pulumi-hcl/pkg/hcl/run"
	"github.com/pulumi/pulumi/sdk/v3/go/common/util/rpcutil"
	"github.com/pulumi/pulumi/sdk/v3/go/propertyrpc"
	pulumirpc "github.com/pulumi/pulumi/sdk/v3/proto/go"
	"google.golang.org/grpc"
	"google.golang.org/protobuf/proto"
)

// callbackServer hosts a gRPC Callbacks service. Each registered callback is
// keyed by a UUID token; the Pulumi engine invokes the callback by sending an
// Invoke RPC with that token.
type callbackServer struct {
	pulumirpc.UnsafeCallbacksServer

	stop      chan bool
	handle    rpcutil.ServeHandle
	mu        sync.RWMutex
	functions map[string]callbackFunction
}

type callbackFunction func(ctx context.Context, req []byte) (proto.Message, error)

func newCallbackServer() (*callbackServer, error) {
	cs := &callbackServer{
		functions: map[string]callbackFunction{},
		stop:      make(chan bool),
	}

	handle, err := rpcutil.ServeWithOptions(rpcutil.ServeOptions{
		Cancel: cs.stop,
		Init: func(srv *grpc.Server) error {
			pulumirpc.RegisterCallbacksServer(srv, cs)
			return nil
		},
		Options: rpcutil.TracingServerInterceptorOptions(nil),
	})
	if err != nil {
		return nil, fmt.Errorf("starting callback server: %w", err)
	}
	cs.handle = handle
	return cs, nil
}

func (s *callbackServer) Close() error {
	select {
	case s.stop <- true:
	default:
	}
	return nil
}

func (s *callbackServer) register(fn callbackFunction) (*pulumirpc.Callback, error) {
	id, err := uuid.NewRandom()
	if err != nil {
		return nil, err
	}
	token := id.String()
	s.mu.Lock()
	s.functions[token] = fn
	s.mu.Unlock()
	return &pulumirpc.Callback{
		Token:             token,
		Target:            "127.0.0.1:" + strconv.Itoa(s.handle.Port),
		AcceptsByteString: true,
	}, nil
}

func (s *callbackServer) Invoke(
	ctx context.Context, req *pulumirpc.CallbackInvokeRequest,
) (*pulumirpc.CallbackInvokeResponse, error) {
	s.mu.RLock()
	fn, ok := s.functions[req.Token]
	s.mu.RUnlock()
	if !ok {
		return nil, errors.New("callback function not found")
	}
	resp, err := fn(ctx, req.Request)
	if err != nil {
		return nil, err
	}
	bytes, err := proto.Marshal(resp)
	if err != nil {
		return nil, fmt.Errorf("marshaling response: %w", err)
	}
	return &pulumirpc.CallbackInvokeResponse{Response: bytes}, nil
}

// lazyCallbackServer is a callback server created on first use and shared for
// the lifetime of the owning provider, so a `before_delete` hook — which fires
// during a later `destroy --run-program`, after Construct returns — still has a
// live callback server to reach.
type lazyCallbackServer struct {
	mu  sync.Mutex
	cbs *callbackServer
}

func (l *lazyCallbackServer) get() (*callbackServer, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.cbs == nil {
		cbs, err := newCallbackServer()
		if err != nil {
			return nil, err
		}
		l.cbs = cbs
	}
	return l.cbs, nil
}

func (l *lazyCallbackServer) close() {
	l.mu.Lock()
	cbs := l.cbs
	l.cbs = nil
	l.mu.Unlock()
	if cbs != nil {
		_ = cbs.Close()
	}
}

// dispatcherSet hands out one run.DestroyDispatcher per deployment (monitor
// endpoint): one provider process serves several deployments, whose
// Constructs must each share their deployment's dispatcher.
type dispatcherSet struct {
	mu         sync.Mutex
	byEndpoint map[string]*run.DestroyDispatcher
}

func (s *dispatcherSet) get(endpoint string) *run.DestroyDispatcher {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.byEndpoint == nil {
		s.byEndpoint = map[string]*run.DestroyDispatcher{}
	}
	d, ok := s.byEndpoint[endpoint]
	if !ok {
		d = run.NewDestroyDispatcher()
		s.byEndpoint[endpoint] = d
	}
	return d
}

// hooksToProto returns nil for a nil binding so the field is omitted.
func hooksToProto(hooks *run.ResourceHookBinding) *pulumirpc.RegisterResourceRequest_ResourceHooksBinding {
	if hooks == nil {
		return nil
	}
	return &pulumirpc.RegisterResourceRequest_ResourceHooksBinding{
		BeforeCreate: hooks.BeforeCreate,
		BeforeUpdate: hooks.BeforeUpdate,
		AfterCreate:  hooks.AfterCreate,
		AfterUpdate:  hooks.AfterUpdate,
		BeforeDelete: hooks.BeforeDelete,
		AfterDelete:  hooks.AfterDelete,
		OnError:      hooks.OnError,
	}
}

// resourceHookCallback adapts a run.ResourceHookFunction to the proto-level
// hook callback contract.
func resourceHookCallback(fn run.ResourceHookFunction) callbackFunction {
	return func(ctx context.Context, request []byte) (proto.Message, error) {
		var req pulumirpc.ResourceHookRequest
		if err := proto.Unmarshal(request, &req); err != nil {
			return nil, fmt.Errorf("unmarshaling hook request: %w", err)
		}
		args := &run.ResourceHookArgs{
			URN:  req.Urn,
			ID:   req.Id,
			Name: req.Name,
			Type: req.Type,
		}
		if req.NewInputs != nil {
			m, err := propertyrpc.Unmarshal(req.NewInputs)
			if err != nil {
				return nil, fmt.Errorf("unmarshaling new inputs: %w", err)
			}
			args.NewInputs = m
		}
		if req.OldInputs != nil {
			m, err := propertyrpc.Unmarshal(req.OldInputs)
			if err != nil {
				return nil, fmt.Errorf("unmarshaling old inputs: %w", err)
			}
			args.OldInputs = m
		}
		if req.NewOutputs != nil {
			m, err := propertyrpc.Unmarshal(req.NewOutputs)
			if err != nil {
				return nil, fmt.Errorf("unmarshaling new outputs: %w", err)
			}
			args.NewOutputs = m
		}
		if req.OldOutputs != nil {
			m, err := propertyrpc.Unmarshal(req.OldOutputs)
			if err != nil {
				return nil, fmt.Errorf("unmarshaling old outputs: %w", err)
			}
			args.OldOutputs = m
		}
		if err := fn(ctx, args); err != nil {
			return &pulumirpc.ResourceHookResponse{Error: err.Error()}, nil
		}
		return &pulumirpc.ResourceHookResponse{}, nil
	}
}
