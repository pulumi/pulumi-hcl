# tfcompat test cases

Each `*_test.go` file in this directory pairs a Terraform program (under
`testdata/cases/<case-name>/`) with both execution paths and asserts they agree
on outputs and on the set of provider CRUD calls (or their exact sequence,
with `Case.OrderDeterministic`).

The harness, recorder, and reusable providers live in
[`../testutil/tfcompat/`](../testutil/tfcompat/README.md) — start there for how the comparison works and how
to add a new in-memory provider.

## Layout

Program files always live on disk; the Go test carries only behavior
(providers, config, per-stage mode/error/output assertions).

```
tfcompat/
├── *_test.go                       # one test per case
└── testdata/
    └── cases/
        ├── <case-name>/
        │   └── *.tf                # flat case: one file set
        └── <staged-case-name>/
            ├── 0/
            │   └── *.tf            # numbered case: one file set per stage
            └── 1/
                └── *.tf
```

A test's `<case-name>` argument to `tfcompat.RunCase` selects the matching
directory under `testdata/cases/`. Every regular file in the case directory is
loaded and fed verbatim to both paths.

## Stages

Each case runs as one or more *stages* — sequential operations against the
same stack. A stage is an apply by default; `Case.Stages` attaches optional
per-stage behavior, matched positionally against the stages loaded from disk:

```go
type Stage struct {
    Mode         StageMode // StageApply (default), StagePreview, StageDestroy
    ExpectErr    string    // both runtimes must fail with this substring
    AssertOutput func(t *testing.T, output string) // runs against each runtime's apply output
}
```

The case directory's shape decides where the file sets come from:

- **Flat directory** — one file set. The case runs `max(1, len(Stages))`
  stages, every stage over the same files. Use multiple `Stages` entries to
  drive the same program through several operations, e.g. preview then apply:

  ```go
  tfcompat.RunCase(t, "l2_check_unknown_deferred", tfcompat.Case{
      Providers: []tfcompat.Provider{{Name: "simple", Factory: providers.SimpleProvider}},
      Stages: []tfcompat.Stage{
          {Mode: tfcompat.StagePreview},
          {},
      },
  })
  ```

- **Numbered stage subdirs** (`0/`, `1/`, ...) — one file set per subdir, for
  tests whose program *changes* between operations (e.g.
  `lifecycle.replace_triggered_by`). `Case.Stages`, if set, must have exactly
  one entry per subdir.

Outputs, provider operations, and `AssertState` are compared after the last
successful apply stage.

## Runtime values

Program files are committed verbatim — never generate program text in Go. A
value only known at runtime (a port, a temp path, a credential) enters through
`Case.Config`: declare a variable in the program and set it in the map. Config
reaches both paths identically (`-var` flags for tofu, stack config for
pulumi), and values are converted per the variable's declared type, so
`variable "port" { type = number }` works from a string config entry.

```go
tfcompat.RunCase(t, "l1_file_tilde", tfcompat.Case{
    Config: map[string]string{"name": name},
})
```

## Running

```bash
go test ./tests/tfcompat/ -v -count=1
```

Run a single case:

```bash
go test ./tests/tfcompat/ -run TestL2SimpleResource -v -count=1
```

Requires `tofu` on `PATH` (or set `TF_COMMAND_OVERRIDE=terraform`).

## Adding a case

1. Create `testdata/cases/<case-name>/main.tf` (plus any other `.tf` files the
   case needs — every file in the directory is loaded).
2. Add `<case-name>_test.go`:

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

3. Reuse a provider from `../testutil/tfcompat/providers/` if one fits;
   otherwise add a new in-memory provider there rather than inline.

## Naming

- `l1_*` — no provider code is exercised (literals, expressions, built-in
  functions).
- `l2_*` — provider code is exercised (resources, data sources, computed
  outputs).

Tests should be named after the noun under test, not the behavior under test. For example,
to test that variable defaults are applied correctly, we the test might be called
`l1_var`, not `l1_var_defaults_applied`.

Tests must never mention the real-world providers or fields that motivated
them (no `aws`, `cluster`, `oidc`, `issuer`, ...). All names are generic and
describe the *shape* of what they are: prefer field names like `attr`,
`block`, `nested_block` and resource names like `pfx_res` or
`simple_resource` over domain nouns like `record`, `cluster`, or `rule`.

## Scope

Cases here must be valid Terraform programs — both paths run the same `.tf`
files. Pulumi-only constructs (component / package blocks, Pulumi-specific
resource options) belong in the language-host test suite. Provider setups a
tf-compatible program cannot produce (e.g. `Customize` hooks on the bridged
`ProviderInfo`) belong in the Pulumi-only `putest` suite under
`tests/putest/`.
