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

package providers

import (
	"context"
	"fmt"
	"sort"
	"strings"

	"github.com/hashicorp/terraform-plugin-sdk/v2/diag"
	"github.com/hashicorp/terraform-plugin-sdk/v2/helper/schema"
)

// BlockyProvider exposes block-shaped TF attributes that the bridge projects
// in different ways:
//
//   - `settings` is TypeList MaxItems=1 with a Resource Elem → bridge flattens
//     it to a single Pulumi object. The TF source writes a block.
//   - `tag` is TypeList with no MaxItems → bridge keeps as a list of objects.
//     The TF source writes one block per element.
//   - `policy` is TypeList MaxItems=1 with a nested `rule` block (also
//     MaxItems=1) → exercises nested singular-block flattening.
//   - `filter` is TypeSet with no MaxItems → bridge keeps as a set of objects
//     and pluralizes the name to `filters`. The TF source writes one block per
//     element, mirroring the `filter` block in real providers (e.g. the
//     `aws_ami` data source).
//
// Each value contributes to the resource's `summary` output, so tests can
// assert the engine fed inputs through to the provider correctly.
func BlockyProvider() *schema.Provider {
	return &schema.Provider{
		DataSourcesMap: map[string]*schema.Resource{
			// blocky_image mirrors the `aws_ami` data source: a repeating
			// TypeSet `filter` block the bridge pluralizes to `filters`.
			"blocky_image": {
				Schema: map[string]*schema.Schema{
					"filter": {
						Type:     schema.TypeSet,
						Optional: true,
						Elem: &schema.Resource{
							Schema: map[string]*schema.Schema{
								"name":   {Type: schema.TypeString, Required: true},
								"values": {Type: schema.TypeString, Required: true},
							},
						},
					},
					"summary": {Type: schema.TypeString, Computed: true},
				},
				ReadContext: func(_ context.Context, d *schema.ResourceData, _ any) diag.Diagnostics {
					d.SetId("blocky-image-id")
					var filters []string
					if set, ok := d.Get("filter").(*schema.Set); ok {
						for _, f := range set.List() {
							if m, ok := f.(map[string]any); ok {
								filters = append(filters, fmt.Sprintf("%v=%v", m["name"], m["values"]))
							}
						}
					}
					sort.Strings(filters)
					return diag.FromErr(d.Set("summary", "filters=["+strings.Join(filters, ",")+"]"))
				},
			},
		},
		ResourcesMap: map[string]*schema.Resource{
			"blocky_thing": {
				Schema: map[string]*schema.Schema{
					"name": {Type: schema.TypeString, Required: true},
					// A MaxItems=1 list with a scalar Elem is an attribute in
					// TF (`alias = ["x"]`) that the bridge flattens to a plain
					// string.
					"alias": {
						Type:     schema.TypeList,
						Optional: true,
						MaxItems: 1,
						Elem:     &schema.Schema{Type: schema.TypeString},
					},
					// A MaxItems=1 list whose Elem is itself a list flattens
					// to a plain list — the flattened value is collection-
					// shaped, unlike alias's scalar.
					"pair": {
						Type:     schema.TypeList,
						Optional: true,
						MaxItems: 1,
						Elem: &schema.Schema{
							Type: schema.TypeList,
							Elem: &schema.Schema{Type: schema.TypeString},
						},
					},
					"settings": {
						Type:     schema.TypeList,
						Optional: true,
						MaxItems: 1,
						Elem: &schema.Resource{
							Schema: map[string]*schema.Schema{
								"mode":    {Type: schema.TypeString, Required: true},
								"verbose": {Type: schema.TypeBool, Optional: true},
							},
						},
					},
					"tag": {
						Type:     schema.TypeList,
						Optional: true,
						Elem: &schema.Resource{
							Schema: map[string]*schema.Schema{
								"key":   {Type: schema.TypeString, Required: true},
								"value": {Type: schema.TypeString, Required: true},
								"note":  {Type: schema.TypeString, Optional: true},
							},
						},
					},
					"policy": {
						Type:     schema.TypeList,
						Optional: true,
						MaxItems: 1,
						Elem: &schema.Resource{
							Schema: map[string]*schema.Schema{
								"effect": {Type: schema.TypeString, Required: true},
								"rule": {
									Type:     schema.TypeList,
									Optional: true,
									MaxItems: 1,
									Elem: &schema.Resource{
										Schema: map[string]*schema.Schema{
											"action":   {Type: schema.TypeString, Required: true},
											"resource": {Type: schema.TypeString, Required: true},
										},
									},
								},
							},
						},
					},
					"filter": {
						Type:     schema.TypeSet,
						Optional: true,
						Elem: &schema.Resource{
							Schema: map[string]*schema.Schema{
								"name":   {Type: schema.TypeString, Required: true},
								"values": {Type: schema.TypeString, Required: true},
							},
						},
					},
					"summary": {Type: schema.TypeString, Computed: true},
				},
				CreateContext: func(_ context.Context, d *schema.ResourceData, _ any) diag.Diagnostics {
					d.SetId("blocky-id")
					name, _ := d.Get("name").(string)
					parts := []string{"name=" + name}

					if list, ok := d.Get("settings").([]any); ok && len(list) > 0 {
						if m, ok := list[0].(map[string]any); ok {
							parts = append(parts, fmt.Sprintf("settings.mode=%v", m["mode"]))
							parts = append(parts, fmt.Sprintf("settings.verbose=%v", m["verbose"]))
						}
					}

					if list, ok := d.Get("tag").([]any); ok {
						var tags []string
						for _, t := range list {
							if m, ok := t.(map[string]any); ok {
								tag := fmt.Sprintf("%v=%v", m["key"], m["value"])
								if note, ok := m["note"].(string); ok && note != "" {
									tag += "(" + note + ")"
								}
								tags = append(tags, tag)
							}
						}
						sort.Strings(tags)
						parts = append(parts, "tags=["+strings.Join(tags, ",")+"]")
					}

					if list, ok := d.Get("policy").([]any); ok && len(list) > 0 {
						if m, ok := list[0].(map[string]any); ok {
							parts = append(parts, fmt.Sprintf("policy.effect=%v", m["effect"]))
							if rules, ok := m["rule"].([]any); ok && len(rules) > 0 {
								if r, ok := rules[0].(map[string]any); ok {
									parts = append(parts,
										fmt.Sprintf("policy.rule.action=%v", r["action"]),
										fmt.Sprintf("policy.rule.resource=%v", r["resource"]))
								}
							}
						}
					}

					if set, ok := d.Get("filter").(*schema.Set); ok && set.Len() > 0 {
						var filters []string
						for _, f := range set.List() {
							if m, ok := f.(map[string]any); ok {
								filters = append(filters, fmt.Sprintf("%v=%v", m["name"], m["values"]))
							}
						}
						sort.Strings(filters)
						parts = append(parts, "filters=["+strings.Join(filters, ",")+"]")
					}

					return diag.FromErr(d.Set("summary", strings.Join(parts, "|")))
				},
				ReadContext:   func(_ context.Context, _ *schema.ResourceData, _ any) diag.Diagnostics { return nil },
				UpdateContext: func(_ context.Context, _ *schema.ResourceData, _ any) diag.Diagnostics { return nil },
				DeleteContext: func(_ context.Context, _ *schema.ResourceData, _ any) diag.Diagnostics { return nil },
			},
			// blocky_single has a TypeSet MaxItems=1 block, which the bridge
			// flattens to one object like a TypeList MaxItems=1 block.
			"blocky_single": {
				Schema: map[string]*schema.Schema{
					"opt": {
						Type:     schema.TypeSet,
						Optional: true,
						MaxItems: 1,
						Elem: &schema.Resource{
							Schema: map[string]*schema.Schema{
								"label": {Type: schema.TypeString, Required: true},
							},
						},
					},
				},
				CreateContext: func(_ context.Context, d *schema.ResourceData, _ any) diag.Diagnostics {
					d.SetId("blocky-single-id")
					return nil
				},
				ReadContext:   func(_ context.Context, _ *schema.ResourceData, _ any) diag.Diagnostics { return nil },
				UpdateContext: func(_ context.Context, _ *schema.ResourceData, _ any) diag.Diagnostics { return nil },
				DeleteContext: func(_ context.Context, _ *schema.ResourceData, _ any) diag.Diagnostics { return nil },
			},
		},
	}
}
