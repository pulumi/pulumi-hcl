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

package run

import (
	"fmt"
	"maps"
	"path/filepath"
	"slices"
	"strings"

	"github.com/pulumi/pulumi-hcl/pkg/hcl/packages"
	"github.com/pulumi/pulumi/sdk/v3/go/property"
)

const (
	// TerraformStatePackage / TerraformStatePackageVersion is the external
	// pulumi-terraform package that provides the state-reference invokes.
	TerraformStatePackage        = "terraform"
	TerraformStatePackageVersion = "6.1.0"
)

// lowerRemoteStateInvoke rewrites a terraform_remote_state invoke into the matching
// pulumi-terraform state-reference invoke: the local backend uses getLocalReference
// and the remote backend uses getRemoteReference. The pulumi-terraform package
// serves no other backend, so any other backend is rejected; support can be added
// as the package gains backends.
//
// The top-level `workspace` is resolved the way OpenTofu's backends do, since
// neither invoke takes a workspace name. On the local backend a non-default
// workspace's state lives at `<workspace_dir>/<workspace>/terraform.tfstate`
// (`path` applies only to the default workspace), so it is resolved into the
// invoke's `path`. On the remote backend, config.workspaces.prefix makes the
// read workspace `<prefix><workspace>` (combined into the invoke's
// `workspaces.name`); with config.workspaces.name only the implicit "default"
// workspace exists, so `workspace` must be "default".
//
// `defaults` is returned for applyRemoteStateDefaults to overlay on the invoke
// result; it is the zero Map when absent.
func lowerRemoteStateInvoke(tfType string, req InvokeRequest) (InvokeRequest, property.Map, error) {
	if tfType != packages.RemoteStateType {
		return req, property.Map{}, nil
	}

	var defaults property.Map
	if v, ok := req.Args.GetOk("defaults"); ok && !v.IsNull() {
		if !v.IsMap() {
			return req, property.Map{}, fmt.Errorf("terraform_remote_state: %q must be an object", "defaults")
		}
		defaults = v.AsMap()
	}

	backend := ""
	if b, ok := req.Args.GetOk("backend"); ok && b.IsString() {
		backend = b.AsString()
	}

	workspace, hasWorkspace := "", false
	if w, ok := req.Args.GetOk("workspace"); ok && w.IsString() {
		workspace, hasWorkspace = w.AsString(), true
	}

	var token, desc string
	var fields map[string]string // recognized config keys -> invoke argument names
	switch backend {
	case "local":
		token = packages.LocalReferenceToken
		desc = "the local backend"
		fields = map[string]string{"path": "path", "workspace_dir": "workspaceDir"}
	case "remote":
		token = packages.RemoteReferenceToken
		desc = fmt.Sprintf("backend %q", backend)
		fields = map[string]string{
			"organization": "organization",
			"hostname":     "hostname",
			"token":        "token",
			"workspaces":   "workspaces",
		}
	default:
		return req, property.Map{}, fmt.Errorf(
			"terraform_remote_state: backend %q is not supported (supported backends: local, remote)", backend,
		)
	}

	args := map[string]property.Value{}
	var unexpected []string
	if cfg, ok := req.Args.GetOk("config"); ok && cfg.IsMap() {
		for k, v := range cfg.AsMap().All() {
			if argKey, ok := fields[k]; ok {
				args[argKey] = v
			} else {
				unexpected = append(unexpected, k)
			}
		}
	}
	if len(unexpected) > 0 {
		slices.Sort(unexpected)
		return req, property.Map{}, fmt.Errorf(
			"terraform_remote_state: %s does not read config field(s) %v (supported: %s)",
			desc, unexpected, strings.Join(slices.Sorted(maps.Keys(fields)), ", "),
		)
	}

	// Resolve the top-level `workspace` the way OpenTofu's backends do:
	//   - local: a non-default workspace's state is at
	//     `<workspace_dir>/<workspace>/terraform.tfstate` and `path` applies only
	//     to the default workspace; "default" (or "") reads as if it were absent
	//   - remote: config.workspaces.prefix + workspace -> read `<prefix><workspace>`;
	//     config.workspaces.name exposes only the implicit "default" workspace,
	//     so workspace must be "default" (or absent); anything else is an error
	if hasWorkspace {
		switch backend {
		case "local":
			if workspace != "default" && workspace != "" {
				dir := "terraform.tfstate.d"
				if d, ok := args["workspaceDir"]; ok && d.IsString() {
					dir = d.AsString()
				}
				delete(args, "workspaceDir")
				args["path"] = property.New(filepath.Join(dir, workspace, "terraform.tfstate"))
			}
		case "remote":
			ws, ok := args["workspaces"]
			if !ok || !ws.IsMap() {
				return req, property.Map{}, fmt.Errorf("terraform_remote_state: workspace requires config.workspaces.prefix")
			}
			switch wsMap := ws.AsMap(); {
			case wsMap.Get("name").IsString():
				if workspace != "default" {
					return req, property.Map{}, fmt.Errorf(
						"terraform_remote_state: workspace %q is invalid with config.workspaces.name (only \"default\" is)", workspace,
					)
				}
			case wsMap.Get("prefix").IsString():
				args["workspaces"] = property.New(map[string]property.Value{"name": property.New(wsMap.Get("prefix").AsString() + workspace)})
			default:
				return req, property.Map{}, fmt.Errorf("terraform_remote_state: workspace requires config.workspaces.prefix")
			}
		}
	}

	req.Token = token
	req.Args = property.NewMap(args)
	return req, defaults, nil
}

// remoteStateResult builds the terraform_remote_state data source's object from
// the state-reference invoke's result. The data source's own `backend`,
// `config`, `workspace` and `defaults` arguments are readable attributes of it,
// so they are stored back alongside `outputs`; the invoke reports only the
// outputs. Absent arguments read as null.
func remoteStateResult(args, defaults, ret property.Map) property.Map {
	ret = applyRemoteStateDefaults(defaults, ret)
	for _, k := range []string{"backend", "config", "workspace", "defaults"} {
		ret = ret.Set(k, args.Get(k))
	}
	return ret
}

// applyRemoteStateDefaults overlays the data source's `defaults` beneath the
// outputs returned by the state-reference invoke: each default supplies the
// value for an output the referenced state does not define, matching OpenTofu's
// per-output fallback. ret is returned unchanged when there are no defaults.
func applyRemoteStateDefaults(defaults, ret property.Map) property.Map {
	if defaults.Len() == 0 {
		return ret
	}
	merged := maps.Collect(defaults.All())
	if out, ok := ret.GetOk("outputs"); ok && out.IsMap() {
		maps.Insert(merged, out.AsMap().All())
	}
	return ret.Set("outputs", property.New(merged))
}
