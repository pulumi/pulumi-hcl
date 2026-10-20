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
	"slices"
	"strings"

	"github.com/hashicorp/terraform-plugin-framework/attr"
	"github.com/hashicorp/terraform-plugin-framework/datasource"
	dschema "github.com/hashicorp/terraform-plugin-framework/datasource/schema"
	"github.com/hashicorp/terraform-plugin-framework/diag"
	"github.com/hashicorp/terraform-plugin-framework/function"
	"github.com/hashicorp/terraform-plugin-framework/provider"
	pschema "github.com/hashicorp/terraform-plugin-framework/provider/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource"
	rschema "github.com/hashicorp/terraform-plugin-framework/resource/schema"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/planmodifier"
	"github.com/hashicorp/terraform-plugin-framework/resource/schema/stringplanmodifier"
	"github.com/hashicorp/terraform-plugin-framework/types"
)

// PFXProvider is a minimal terraform-plugin-framework provider: two resources
// holding a single string, and one data source returning a constant. It
// exists so tfcompat covers providers built on the plugin framework rather
// than terraform-plugin-sdk/v2. pfx_widget accepts a `moved` from pfx_thing,
// which no terraform-plugin-sdk/v2 provider can (its MoveResourceState
// unconditionally errors).
func PFXProvider() provider.Provider { return &pfxProvider{} }

type pfxProvider struct{}

var (
	_ provider.Provider              = (*pfxProvider)(nil)
	_ provider.ProviderWithFunctions = (*pfxProvider)(nil)
)

func (p *pfxProvider) Metadata(_ context.Context, _ provider.MetadataRequest, resp *provider.MetadataResponse) {
	resp.TypeName = "pfx"
}

func (p *pfxProvider) Schema(_ context.Context, _ provider.SchemaRequest, resp *provider.SchemaResponse) {
	resp.Schema = pschema.Schema{
		Attributes: map[string]pschema.Attribute{
			"registries": pschema.ListNestedAttribute{
				Optional: true,
				NestedObject: pschema.NestedAttributeObject{
					Attributes: map[string]pschema.Attribute{
						"url": pschema.StringAttribute{Optional: true},
					},
				},
			},
		},
	}
}

type pfxProviderModel struct {
	Registries []struct {
		URL types.String `tfsdk:"url"`
	} `tfsdk:"registries"`
}

// Configure hands the configured registry URLs to the data sources, so
// pfx_set_lookup can echo them.
func (p *pfxProvider) Configure(ctx context.Context, req provider.ConfigureRequest, resp *provider.ConfigureResponse) {
	var config pfxProviderModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &config)...)
	if resp.Diagnostics.HasError() {
		return
	}
	urls := make([]string, 0, len(config.Registries))
	for _, r := range config.Registries {
		urls = append(urls, r.URL.ValueString())
	}
	resp.DataSourceData = urls
}

func (p *pfxProvider) Resources(context.Context) []func() resource.Resource {
	return []func() resource.Resource{
		func() resource.Resource { return &pfxThing{} },
		func() resource.Resource { return &pfxWidget{} },
		func() resource.Resource { return &pfxRes{} },
		func() resource.Resource { return &pfxObj{} },
		func() resource.Resource { return &pfxMatrix{} },
		func() resource.Resource { return &pfxFlat{} },
		func() resource.Resource { return &pfxAnon{} },
	}
}

func (p *pfxProvider) DataSources(context.Context) []func() datasource.DataSource {
	return []func() datasource.DataSource{
		func() datasource.DataSource { return &pfxLookup{} },
		func() datasource.DataSource { return &pfxObjLookup{} },
		func() datasource.DataSource { return &pfxSetLookup{} },
	}
}

func (p *pfxProvider) Functions(context.Context) []func() function.Function {
	return []func() function.Function{
		func() function.Function { return &pfxConcatStr{} },
		func() function.Function { return &pfxJoinStr{} },
	}
}

// pfxConcatStr is a provider-defined function: concat_str(first, second)
// concatenates two strings. second allows null (projected as an optional
// argument); first == "boom" returns a function error, so error propagation
// can be compared across runtimes.
type pfxConcatStr struct{}

var _ function.Function = (*pfxConcatStr)(nil)

func (f *pfxConcatStr) Metadata(_ context.Context, _ function.MetadataRequest, resp *function.MetadataResponse) {
	resp.Name = "concat_str"
}

func (f *pfxConcatStr) Definition(_ context.Context, _ function.DefinitionRequest, resp *function.DefinitionResponse) {
	resp.Definition = function.Definition{
		Parameters: []function.Parameter{
			function.StringParameter{Name: "first"},
			function.StringParameter{Name: "second", AllowNullValue: true},
		},
		Return: function.StringReturn{},
	}
}

func (f *pfxConcatStr) Run(ctx context.Context, req function.RunRequest, resp *function.RunResponse) {
	var first string
	var second *string
	resp.Error = function.ConcatFuncErrors(resp.Error, req.Arguments.Get(ctx, &first, &second))
	if resp.Error != nil {
		return
	}
	if first == "boom" {
		resp.Error = function.NewFuncError("concat_str: intentional failure")
		return
	}
	out := first
	if second != nil {
		out += *second
	}
	resp.Error = function.ConcatFuncErrors(resp.Error, resp.Result.Set(ctx, out))
}

type pfxThing struct{}

var _ resource.Resource = (*pfxThing)(nil)

type pfxThingModel struct {
	ID   types.String `tfsdk:"id"`
	Name types.String `tfsdk:"name"`
}

func (r *pfxThing) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_thing"
}

// pfxThingSchema is shared by pfx_thing and pfx_widget: the cross-type `moved`
// test moves state between two resources of identical shape.
func pfxThingSchema() rschema.Schema {
	return rschema.Schema{
		Attributes: map[string]rschema.Attribute{
			"id": rschema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"name": rschema.StringAttribute{Required: true},
		},
	}
}

func (r *pfxThing) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = pfxThingSchema()
}

func (r *pfxThing) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan pfxThingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue("pfx-id")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxThing) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {}

func (r *pfxThing) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan pfxThingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxThing) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {}

// pfxWidget is pfx_thing under another type name. It implements MoveState so a
// `moved { from = pfx_thing.x, to = pfx_widget.x }` is a state-only move.
type pfxWidget struct{}

var (
	_ resource.Resource              = (*pfxWidget)(nil)
	_ resource.ResourceWithMoveState = (*pfxWidget)(nil)
)

func (r *pfxWidget) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_widget"
}

func (r *pfxWidget) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = pfxThingSchema()
}

func (r *pfxWidget) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan pfxThingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue("pfx-widget-id")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxWidget) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {}

func (r *pfxWidget) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan pfxThingModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxWidget) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

func (r *pfxWidget) MoveState(context.Context) []resource.StateMover {
	sourceSchema := pfxThingSchema()
	return []resource.StateMover{{
		SourceSchema: &sourceSchema,
		StateMover: func(ctx context.Context, req resource.MoveStateRequest, resp *resource.MoveStateResponse) {
			if req.SourceTypeName != "pfx_thing" || req.SourceState == nil {
				return
			}
			var src pfxThingModel
			resp.Diagnostics.Append(req.SourceState.Get(ctx, &src)...)
			if resp.Diagnostics.HasError() {
				return
			}
			resp.Diagnostics.Append(resp.TargetState.Set(ctx, &src)...)
		},
	}}
}

// pfxJoinStr is a variadic provider-defined function: join_str(separator,
// parts...) joins the trailing arguments with the separator. It exists to
// compare Terraform's spread call syntax across runtimes.
type pfxJoinStr struct{}

var _ function.Function = (*pfxJoinStr)(nil)

func (f *pfxJoinStr) Metadata(_ context.Context, _ function.MetadataRequest, resp *function.MetadataResponse) {
	resp.Name = "join_str"
}

func (f *pfxJoinStr) Definition(_ context.Context, _ function.DefinitionRequest, resp *function.DefinitionResponse) {
	resp.Definition = function.Definition{
		Parameters: []function.Parameter{
			function.StringParameter{Name: "separator"},
		},
		VariadicParameter: function.StringParameter{Name: "parts"},
		Return:            function.StringReturn{},
	}
}

func (f *pfxJoinStr) Run(ctx context.Context, req function.RunRequest, resp *function.RunResponse) {
	var separator string
	var parts []string
	resp.Error = function.ConcatFuncErrors(resp.Error, req.Arguments.Get(ctx, &separator, &parts))
	if resp.Error != nil {
		return
	}
	resp.Error = function.ConcatFuncErrors(resp.Error, resp.Result.Set(ctx, strings.Join(parts, separator)))
}

// pfxRes carries a computed list-nested attribute whose element holds
// another computed list-nested attribute.
type pfxRes struct{}

var _ resource.Resource = (*pfxRes)(nil)

func (r *pfxRes) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_res"
}

func (r *pfxRes) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = rschema.Schema{
		Attributes: map[string]rschema.Attribute{
			"id": rschema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"attr": rschema.ListNestedAttribute{
				Computed: true,
				NestedObject: rschema.NestedAttributeObject{
					Attributes: map[string]rschema.Attribute{
						"nested_attr": rschema.ListNestedAttribute{
							Computed: true,
							NestedObject: rschema.NestedAttributeObject{
								Attributes: map[string]rschema.Attribute{
									"value": rschema.StringAttribute{Computed: true},
								},
							},
						},
					},
				},
			},
		},
	}
}

type pfxResModel struct {
	ID   types.String `tfsdk:"id"`
	Attr types.List   `tfsdk:"attr"`
}

func pfxResAttrValue(ctx context.Context) (types.List, diag.Diagnostics) {
	nestedType := types.ObjectType{AttrTypes: map[string]attr.Type{"value": types.StringType}}
	attrType := types.ObjectType{AttrTypes: map[string]attr.Type{"nested_attr": types.ListType{ElemType: nestedType}}}
	nested, diags := types.ObjectValue(nestedType.AttrTypes, map[string]attr.Value{
		"value": types.StringValue("computed-value"),
	})
	if diags.HasError() {
		return types.List{}, diags
	}
	nestedList, d := types.ListValue(nestedType, []attr.Value{nested})
	diags.Append(d...)
	if diags.HasError() {
		return types.List{}, diags
	}
	attrVal, d := types.ObjectValue(attrType.AttrTypes, map[string]attr.Value{"nested_attr": nestedList})
	diags.Append(d...)
	if diags.HasError() {
		return types.List{}, diags
	}
	attrList, d := types.ListValue(attrType, []attr.Value{attrVal})
	diags.Append(d...)
	return attrList, diags
}

func (r *pfxRes) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	attrVal, diags := pfxResAttrValue(ctx)
	resp.Diagnostics.Append(diags...)
	if resp.Diagnostics.HasError() {
		return
	}
	state := pfxResModel{ID: types.StringValue("pfx-res-id"), Attr: attrVal}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

func (r *pfxRes) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {}

func (r *pfxRes) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan pfxResModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxRes) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {
}

// pfxObj carries an optional object-typed attribute whose `item` field is a
// list of strings — the bridge pluralizes the nested name to `items`.
type pfxObj struct{}

var _ resource.Resource = (*pfxObj)(nil)

type pfxObjModel struct {
	ID      types.String `tfsdk:"id"`
	ObjAttr types.Object `tfsdk:"obj_attr"`
}

func (r *pfxObj) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_obj"
}

func (r *pfxObj) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = rschema.Schema{
		Attributes: map[string]rschema.Attribute{
			"id": rschema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"obj_attr": rschema.SingleNestedAttribute{
				Optional: true,
				Attributes: map[string]rschema.Attribute{
					"item": rschema.ListAttribute{ElementType: types.StringType, Optional: true},
				},
			},
		},
	}
}

func (r *pfxObj) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan pfxObjModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue("pfx-obj-id")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxObj) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {}

func (r *pfxObj) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan pfxObjModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxObj) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {}

// pfxFlat carries an optional list-nested attribute. Unlike an SDKv2 block,
// TF assigns it with attribute syntax (`settings = [{...}]`); tests flatten
// it to a single Pulumi object with a MaxItemsOne override, the projection
// real bridged providers apply to single-element list attributes.
type pfxFlat struct{}

var _ resource.Resource = (*pfxFlat)(nil)

type pfxFlatModel struct {
	ID       types.String `tfsdk:"id"`
	Settings types.List   `tfsdk:"settings"`
}

func (r *pfxFlat) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_flat"
}

func (r *pfxFlat) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = rschema.Schema{
		Attributes: map[string]rschema.Attribute{
			"id": rschema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"settings": rschema.ListNestedAttribute{
				Optional: true,
				NestedObject: rschema.NestedAttributeObject{
					Attributes: map[string]rschema.Attribute{
						"enabled": rschema.BoolAttribute{Optional: true},
					},
				},
			},
		},
	}
}

func (r *pfxFlat) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan pfxFlatModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue("pfx-flat-id")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxFlat) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {}

func (r *pfxFlat) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan pfxFlatModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxFlat) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {}

// pfxObjLookup is the data-source counterpart of pfxObj: an object-typed
// input attribute whose `item` field is a list of strings (bridged as
// `items`), echoed back through the computed `value`.
type pfxObjLookup struct{}

var _ datasource.DataSource = (*pfxObjLookup)(nil)

type pfxObjLookupModel struct {
	ObjAttr types.Object `tfsdk:"obj_attr"`
	Value   types.String `tfsdk:"value"`
}

func (d *pfxObjLookup) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_obj_lookup"
}

func (d *pfxObjLookup) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = dschema.Schema{
		Attributes: map[string]dschema.Attribute{
			"obj_attr": dschema.SingleNestedAttribute{
				Optional: true,
				Attributes: map[string]dschema.Attribute{
					"item": dschema.ListAttribute{ElementType: types.StringType, Optional: true},
				},
			},
			"value": dschema.StringAttribute{Computed: true},
		},
	}
}

func (d *pfxObjLookup) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var state pfxObjLookupModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var items []string
	if !state.ObjAttr.IsNull() {
		resp.Diagnostics.Append(state.ObjAttr.Attributes()["item"].(types.List).ElementsAs(ctx, &items, false)...)
		if resp.Diagnostics.HasError() {
			return
		}
	}
	state.Value = types.StringValue(strings.Join(items, ","))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

type pfxLookup struct{}

var _ datasource.DataSource = (*pfxLookup)(nil)

type pfxLookupModel struct {
	Value types.String `tfsdk:"value"`
}

func (d *pfxLookup) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_lookup"
}

func (d *pfxLookup) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = dschema.Schema{
		Attributes: map[string]dschema.Attribute{
			"value": dschema.StringAttribute{Computed: true},
		},
	}
}

func (d *pfxLookup) Read(ctx context.Context, _ datasource.ReadRequest, resp *datasource.ReadResponse) {
	state := pfxLookupModel{Value: types.StringValue("pfx-value")}
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}

// pfxMatrix carries a single map-of-list-of-object attribute. The values of
// the map are lists, so two keys may hold lists of different lengths while the
// attribute as a whole stays one well-typed map(list(object)). Create and
// Update echo the plan into state so a stack output shows what the runtime
// planned.
type pfxMatrix struct{}

var _ resource.Resource = (*pfxMatrix)(nil)

type pfxMatrixModel struct {
	ID     types.String `tfsdk:"id"`
	Matrix types.Map    `tfsdk:"matrix"`
}

func (r *pfxMatrix) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_matrix"
}

func (r *pfxMatrix) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = rschema.Schema{
		Attributes: map[string]rschema.Attribute{
			"id": rschema.StringAttribute{
				Computed:      true,
				PlanModifiers: []planmodifier.String{stringplanmodifier.UseStateForUnknown()},
			},
			"matrix": rschema.MapAttribute{
				Optional: true,
				ElementType: types.ListType{ElemType: types.ObjectType{
					AttrTypes: map[string]attr.Type{"name": types.StringType},
				}},
			},
		},
	}
}

func (r *pfxMatrix) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan pfxMatrixModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	plan.ID = types.StringValue("pfx-matrix-id")
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxMatrix) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {}

func (r *pfxMatrix) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan pfxMatrixModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxMatrix) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {}

// pfxAnon declares no `id` attribute at all — legal for a plugin-framework
// resource, and the shape the bridge answers with its "missing ID" sentinel
// because there is no ID property to project. Its state carries no `id` for
// an import to key on either.
type pfxAnon struct{}

var _ resource.Resource = (*pfxAnon)(nil)

type pfxAnonModel struct {
	Name types.String `tfsdk:"name"`
}

func (r *pfxAnon) Metadata(_ context.Context, req resource.MetadataRequest, resp *resource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_anon"
}

func (r *pfxAnon) Schema(_ context.Context, _ resource.SchemaRequest, resp *resource.SchemaResponse) {
	resp.Schema = rschema.Schema{
		Attributes: map[string]rschema.Attribute{
			"name": rschema.StringAttribute{Required: true},
		},
	}
}

func (r *pfxAnon) Create(ctx context.Context, req resource.CreateRequest, resp *resource.CreateResponse) {
	var plan pfxAnonModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxAnon) Read(_ context.Context, _ resource.ReadRequest, _ *resource.ReadResponse) {}

func (r *pfxAnon) Update(ctx context.Context, req resource.UpdateRequest, resp *resource.UpdateResponse) {
	var plan pfxAnonModel
	resp.Diagnostics.Append(req.Plan.Get(ctx, &plan)...)
	if resp.Diagnostics.HasError() {
		return
	}
	resp.Diagnostics.Append(resp.State.Set(ctx, &plan)...)
}

func (r *pfxAnon) Delete(_ context.Context, _ resource.DeleteRequest, _ *resource.DeleteResponse) {}

// pfxSetLookup carries an optional set-nested attribute. It echoes the sorted
// names of its items and the provider's configured registry URLs.
type pfxSetLookup struct {
	registries []string
}

var _ datasource.DataSourceWithConfigure = (*pfxSetLookup)(nil)

type pfxSetLookupModel struct {
	Items      types.Set    `tfsdk:"items"`
	Names      types.String `tfsdk:"names"`
	Registries types.String `tfsdk:"registries"`
}

func (d *pfxSetLookup) Metadata(_ context.Context, req datasource.MetadataRequest, resp *datasource.MetadataResponse) {
	resp.TypeName = req.ProviderTypeName + "_set_lookup"
}

func (d *pfxSetLookup) Schema(_ context.Context, _ datasource.SchemaRequest, resp *datasource.SchemaResponse) {
	resp.Schema = dschema.Schema{
		Attributes: map[string]dschema.Attribute{
			"items": dschema.SetNestedAttribute{
				Optional: true,
				NestedObject: dschema.NestedAttributeObject{
					Attributes: map[string]dschema.Attribute{
						"name": dschema.StringAttribute{Optional: true},
					},
				},
			},
			"names":      dschema.StringAttribute{Computed: true},
			"registries": dschema.StringAttribute{Computed: true},
		},
	}
}

func (d *pfxSetLookup) Configure(_ context.Context, req datasource.ConfigureRequest, _ *datasource.ConfigureResponse) {
	if urls, ok := req.ProviderData.([]string); ok {
		d.registries = urls
	}
}

func (d *pfxSetLookup) Read(ctx context.Context, req datasource.ReadRequest, resp *datasource.ReadResponse) {
	var state pfxSetLookupModel
	resp.Diagnostics.Append(req.Config.Get(ctx, &state)...)
	if resp.Diagnostics.HasError() {
		return
	}
	var items []struct {
		Name types.String `tfsdk:"name"`
	}
	resp.Diagnostics.Append(state.Items.ElementsAs(ctx, &items, false)...)
	if resp.Diagnostics.HasError() {
		return
	}
	names := make([]string, 0, len(items))
	for _, item := range items {
		names = append(names, item.Name.ValueString())
	}
	slices.Sort(names)
	state.Names = types.StringValue(strings.Join(names, ","))
	state.Registries = types.StringValue(strings.Join(d.registries, ","))
	resp.Diagnostics.Append(resp.State.Set(ctx, &state)...)
}
