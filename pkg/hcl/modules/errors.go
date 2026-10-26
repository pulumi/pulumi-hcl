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

package modules

import (
	"errors"
	"net/http"
)

// Classification sentinels wrapped by loader errors so callers can classify a
// failure with errors.Is instead of parsing message text.
var (
	// ErrNotFound marks a module, version, or subdirectory that does not exist.
	ErrNotFound = errors.New("not found")
	// ErrUnauthenticated marks a registry request whose credentials the
	// registry rejected (HTTP 401).
	ErrUnauthenticated = errors.New("unauthenticated")
	// ErrPermissionDenied marks a registry request rejected despite valid
	// credentials (HTTP 403).
	ErrPermissionDenied = errors.New("permission denied")
	// ErrNoCredentials marks a registry request that the registry refused
	// (HTTP 401 or 403) and that carried no credentials, because none are
	// configured for its host.
	ErrNoCredentials = errors.New("no credentials for registry")
	// ErrTransient marks a failure worth retrying: a network error or a
	// retriable HTTP status from the registry.
	ErrTransient = errors.New("transient failure")
	// ErrInvalid marks a malformed module source or version constraint.
	ErrInvalid = errors.New("invalid")
)

// classifiedError pairs an error with a classification sentinel without
// altering its message.
type classifiedError struct {
	kind error
	err  error
}

func (e classifiedError) Error() string   { return e.err.Error() }
func (e classifiedError) Unwrap() []error { return []error{e.kind, e.err} }

func classified(kind, err error) error { return classifiedError{kind: kind, err: err} }

// classifiedHTTP classifies err by the HTTP status of the failed registry
// request and by whether the request carried credentials, returning err
// unchanged for statuses with no useful classification.
func classifiedHTTP(status int, credentialed bool, err error) error {
	switch {
	case !credentialed && (status == http.StatusUnauthorized || status == http.StatusForbidden):
		return classified(ErrNoCredentials, err)
	case status == http.StatusNotFound:
		return classified(ErrNotFound, err)
	case status == http.StatusUnauthorized:
		return classified(ErrUnauthenticated, err)
	case status == http.StatusForbidden:
		return classified(ErrPermissionDenied, err)
	case status == http.StatusRequestTimeout,
		status == http.StatusTooManyRequests,
		status >= 500:
		return classified(ErrTransient, err)
	default:
		return err
	}
}
