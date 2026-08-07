# tfcompat — Terraform compatibility test harness

`tfcompat` verifies that running a Terraform `.tf` program through the real
Pulumi engine + `pulumi-language-hcl` runtime produces the same observable
behavior as running the same program through `tofu apply`.

Two paths share one wrapped TF provider per name. Both paths see the same
SDK-level CRUD calls; recordings of those calls are compared between paths
along with stack outputs.

## Layout

Tests live in `tests/tfcompat/` (one `_test.go` per case). The shared
harness, reusable providers, and sample testdata index live here under
`tests/testutil/tfcompat/`.

```
tests/
├── tfcompat/                       # one _test.go per case
│   ├── l2_simple_resource_test.go
│   └── testdata/
│       └── cases/
│           └── <case-name>/
│               └── *.tf
└── testutil/
    ├── tfcompat/                   # shared harness (this package)
    │   └── providers/              # reusable in-memory TF providers
    ├── pulexec/                    # `pulumi up` runner
    └── tfexec/                     # `tofu apply` runner + Recorder
```

## What's compared

Each test runs both paths in parallel against the same wrapped providers:

- **Path A** — `tofu apply` against TF providers via reattach
  (`TF_REATTACH_PROVIDERS`).
- **Path B** — `pulumi up` via `pulumi-language-hcl`, where the real
  `terraform-provider` plugin is parameterized through the production
  `pulumi install` flow and reattaches to the same in-memory TF providers
  (`PULUMI_BRIDGE_REATTACH_PROVIDERS`).

Both paths therefore consume the provider exactly the way their production
runtimes do — including the wire-protocol schema the dynamic bridge derives
on the Pulumi side.

The harness asserts equality of two things:

1. **Stack outputs** — `terraform.tfstate` outputs vs. Pulumi stack outputs.
2. **Provider operations** — recordings of every `CreateContext`,
   `ReadContext`, `UpdateContext`, `DeleteContext`, data-source
   `ReadContext`, and provider-function call, with no deduplication.

Recordings are captured at the `*schema.Provider` CRUD boundary so both
transports produce identical shapes when behavior matches.

## Ordering cases

`Case.OrderDeterministic: true` turns the op sequence itself into the
assertion: both runtimes must provoke the same provider calls *in the same
order*. This is how dependency-edge behavior (create serialization, destroy
ordering) is tested — no assertion logic in the provider.

Two rules for such cases:

- **Every recorded op must sit on one dependency chain.** Both runtimes run
  independent ops concurrently, so any two ops not ordered by the program's
  dependency graph record in a racy order and the comparison flakes. This
  includes data-source reads and provider-function calls.
- **Delay the op that must complete first** When the test is ordering focused,
  (`providers.OrderProvider`'s `delay_create`/`delay_delete`). When the edge
  under test is honored, the delay just stretches the serialized sequence; when
  it is missing, the ops run concurrently and the undelayed op reliably records
  ahead of the delayed one — so a regression fails deterministically instead of
  racing.

## Test levels

- **L1** tests do not exercise provider code (literals, expressions,
  built-in functions).
- **L2** tests exercise provider code (custom resources, invokes,
  computed outputs).

Naming follows `pulumi-converter-terraform/tests/conformance/`.

## Running

```bash
go test ./tests/tfcompat/ -v -count=1
```

Requires `tofu` (default) or `terraform` on `PATH`. Override with
`TF_COMMAND_OVERRIDE=terraform`.

## Writing a new case

Program files always live on disk under the case directory; `Case.Stages`
carries optional per-stage behavior (mode, expected error, output assertion)
matched positionally against the disk stages. See
[`tests/tfcompat/README.md`](../../tfcompat/README.md) for the full case
layout, staging rules, and how runtime values flow through `Case.Config`.

1. Create `tests/tfcompat/testdata/cases/<case-name>/main.tf` (plus any
   additional `.tf` or auxiliary files).
2. Add `tests/tfcompat/<case-name>_test.go`:

   ```go
   func TestL2MyCase(t *testing.T) {
       t.Parallel()
       tfcompat.RunCase(t, "<case-name>", tfcompat.Case{
           Providers: []tfcompat.Provider{
               {Name: "simple", Factory: providers.SimpleProvider},
           },
       })
   }
   ```

3. Reuse providers from `testutil/tfcompat/providers` or add a new
   in-memory provider there.

## Why `.tf` (not `.hcl`)

OpenTofu only picks up `.tf` / `.tf.json`. `pulumi-language-hcl`'s parser
also picks up `.tf` so the same file feeds both paths verbatim.

## Scope

Pulumi HCL is a superset of Terraform HCL, so not every well-formed HCL
program is a tfcompat candidate — only the TF-compatible subset is. In
particular:

- A `terraform { required_providers { ... } }` block with a
  `pulumi/*`-style source has no meaning on the TF side; both paths
  discover the provider from the resource type prefix (defaulting to the
  `hashicorp/<name>` source), so fixtures can omit the block.
- Pulumi-only constructs (component / package blocks, Pulumi-specific
  resource options) belong in the language-host test suite, not here.
- Provider setups a tf-compatible program cannot produce — `Customize`
  hooks on the bridged `ProviderInfo` foremost — belong in the
  Pulumi-only `putest` suite (`tests/putest/`,
  `tests/testutil/putest/`), which asserts directly on stack outputs,
  Pulumi state, and recorded provider ops instead of comparing against
  OpenTofu. A tfcompat case skipped as a known divergence of the
  terraform-provider plugin path gets a putest twin pinning the correct
  (OpenTofu-matching) behavior against the linked-in bridge until the
  divergence is fixed.

Only Create + DataSource Read are exercised by the first test. The
recorder shapes for Update/Delete are wired but not yet covered by a
case.
