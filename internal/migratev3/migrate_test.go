package migratev3

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/voocel/ainovel-cli/internal/domain"
	"github.com/voocel/ainovel-cli/internal/projectprofile"
)

func TestDraftVerifyFakeGenerator_WriteBoundaryAndPrivacy(t *testing.T) {
	book, expected := makeFixtureBook(t)
	secret := "sk-test-never-copy-this-secret"
	mustWrite(t, filepath.Join(book, "private", "notes.txt"), []byte(secret+" user:pass token=x"))
	before := mustSnapshot(t, book)

	result, err := Draft(context.Background(), DraftOptions{
		BookDir: book, Generator: FakeGenerator{},
		ExpectedEnrolled: &expected, Now: fixedNow, RandomBytes: fixedRandom,
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.CostUSD != 0 || result.ManifestSHA256 == "" {
		t.Fatalf("unexpected result: %+v", result)
	}
	assertSnapshot(t, book, before)
	if err := noSecretLeak(result.RunDir, []string{secret, "user:pass", "token=x"}); err != nil {
		t.Fatal(err)
	}
	files, err := listRegularFiles(filepath.Join(result.RunDir, "proposals"))
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 42 {
		t.Fatalf("proposal count = %d, want 42", len(files))
	}
	verified, err := Verify(VerifyOptions{BookDir: book, RunDir: result.RunDir, ExpectedEnrolled: &expected})
	if err != nil {
		t.Fatal(err)
	}
	if verified.ManifestSHA256 != result.ManifestSHA256 || verified.LogicalBatches != 42 || verified.TotalScenes != 182 {
		t.Fatalf("unexpected verify result: %+v", verified)
	}
	assertSnapshot(t, book, before)
}

func TestStage3aRejectsProviderConfigBeforeAnySourceOrGeneratorRead(t *testing.T) {
	secret := "sk-provider-config-must-not-be-read"
	config := filepath.Join(t.TempDir(), "provider.json")
	mustWrite(t, config, []byte(`{"api_key":"`+secret+`"}`))
	gen := &mutatingGenerator{mutate: func(_ ChapterRequest, raw []byte) []byte { return raw }}
	result, err := Draft(context.Background(), DraftOptions{
		BookDir: filepath.Join(t.TempDir(), "missing-book"), Generator: gen, ProviderConfigPath: config,
	})
	if err == nil || !strings.Contains(err.Error(), "rejects --provider-config") {
		t.Fatalf("got %v, want Stage 3a provider-config rejection", err)
	}
	if gen.calls != 0 || result.RunDir != "" {
		t.Fatalf("provider config rejection crossed source/generator boundary: calls=%d run=%q", gen.calls, result.RunDir)
	}
	data, readErr := os.ReadFile(config)
	if readErr != nil || !bytes.Contains(data, []byte(secret)) {
		t.Fatalf("provider config changed: err=%v", readErr)
	}
}

func TestApprovedSourceWhitelistIsClosed(t *testing.T) {
	book, expected := makeFixtureBook(t)
	secret := "non-whitelisted-book-secret"
	mustWrite(t, filepath.Join(book, "private", "notes.txt"), []byte(secret))
	source, err := loadSource(book, &expected)
	if err != nil {
		t.Fatal(err)
	}
	want := approvedSourcePaths()
	sort.Strings(want)
	if len(source.Files) != 39 || len(source.Hashes) != 39 || len(want) != 39 {
		t.Fatalf("closed whitelist counts files=%d hashes=%d approved=%d", len(source.Files), len(source.Hashes), len(want))
	}
	for i, rel := range want {
		if source.Hashes[i].Path != rel {
			t.Fatalf("whitelist path[%d]=%q want %q", i, source.Hashes[i].Path, rel)
		}
		if _, ok := source.Files[rel]; !ok {
			t.Fatalf("approved source missing from frozen bytes: %s", rel)
		}
	}
	for rel, data := range source.Files {
		if rel == "private/notes.txt" || bytes.Contains(data, []byte(secret)) {
			t.Fatalf("non-whitelisted content entered frozen source: %s", rel)
		}
	}
	if _, ok := source.FullSnapshot["private/notes.txt"]; !ok {
		t.Fatal("full-book integrity map did not cover non-whitelisted path")
	}
}

func TestDraftGeneratorProtocolRejectsAndLeavesNoManifest(t *testing.T) {
	cases := map[string]func(ChapterRequest, []byte) []byte{
		"invalid-json":  func(_ ChapterRequest, _ []byte) []byte { return []byte(`{"items":[`) },
		"trailing-json": func(_ ChapterRequest, raw []byte) []byte { return append(raw, []byte(` {}`)...) },
		"unknown-id": func(_ ChapterRequest, raw []byte) []byte {
			var env proposalEnvelope
			_ = json.Unmarshal(raw, &env)
			env.Items[0].ID = "ch-99/s-99"
			out, _ := json.Marshal(env)
			return out
		},
		"duplicate-id": func(_ ChapterRequest, raw []byte) []byte {
			var env proposalEnvelope
			_ = json.Unmarshal(raw, &env)
			env.Items = append(env.Items, env.Items[0])
			out, _ := json.Marshal(env)
			return out
		},
		"omitted-id": func(_ ChapterRequest, raw []byte) []byte {
			var env proposalEnvelope
			_ = json.Unmarshal(raw, &env)
			env.Items = env.Items[1:]
			out, _ := json.Marshal(env)
			return out
		},
		"rewrite-preserved": func(_ ChapterRequest, raw []byte) []byte {
			var env proposalEnvelope
			_ = json.Unmarshal(raw, &env)
			env.Items[0].Fields["action"] = "rewritten"
			out, _ := json.Marshal(env)
			return out
		},
		"retained-provider-sentinel": func(_ ChapterRequest, raw []byte) []byte {
			var env proposalEnvelope
			_ = json.Unmarshal(raw, &env)
			for field := range env.Items[0].Fields {
				env.Items[0].Fields[field] = providerFillSentinel
				break
			}
			out, _ := json.Marshal(env)
			return out
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			book, expected := makeFixtureBook(t)
			before := mustSnapshot(t, book)
			gen := &mutatingGenerator{mutate: mutate}
			result, err := Draft(context.Background(), DraftOptions{
				BookDir: book, Generator: gen, MaxAttempts: 1, ExpectedEnrolled: &expected,
			})
			if err == nil {
				t.Fatal("expected protocol rejection")
			}
			if gen.calls != 1 {
				t.Fatalf("generator calls = %d, want 1", gen.calls)
			}
			assertSnapshot(t, book, before)
			if _, statErr := os.Stat(filepath.Join(result.RunDir, "manifest.json")); !os.IsNotExist(statErr) {
				t.Fatalf("failed run unexpectedly has manifest: %v", statErr)
			}
			var exchange providerExchange
			if readErr := readStrictFile(filepath.Join(result.RunDir, "provider", "exchanges", "ch-01-attempt-01.json"), &exchange); readErr != nil {
				t.Fatal(readErr)
			}
			if exchange.Record.Chapter != 1 || exchange.Record.Attempt != 1 || exchange.Record.ErrorCategory != "protocol_error" {
				t.Fatalf("failed exchange did not preserve attempt accounting: %+v", exchange.Record)
			}
		})
	}
}

func TestSourceStrictValidationStopsBeforeGenerator(t *testing.T) {
	cases := map[string]func(*testing.T, string){
		"unknown-field": func(t *testing.T, book string) {
			path := filepath.Join(book, "outline.json")
			data, _ := os.ReadFile(path)
			data = bytes.Replace(data, []byte(`"chapter": 1`), []byte(`"chapter": 1, "unexpected": true`), 1)
			mustWrite(t, path, data)
		},
		"duplicate-key": func(t *testing.T, book string) {
			path := filepath.Join(book, "outline.json")
			data, _ := os.ReadFile(path)
			data = bytes.Replace(data, []byte(`"chapter": 1`), []byte(`"chapter": 1, "chapter": 1`), 1)
			mustWrite(t, path, data)
		},
		"trailing-json": func(t *testing.T, book string) {
			path := filepath.Join(book, "outline.json")
			data, _ := os.ReadFile(path)
			mustWrite(t, path, append(data, []byte(` {}`)...))
		},
		"flat-layered-mismatch": func(t *testing.T, book string) {
			path := filepath.Join(book, "outline.json")
			data, _ := os.ReadFile(path)
			data = bytes.Replace(data, []byte(`"title": "第01章"`), []byte(`"title": "被篡改"`), 1)
			mustWrite(t, path, data)
		},
		"empty-legacy-string": func(t *testing.T, book string) {
			for _, name := range []string{"outline.json", "layered_outline.json"} {
				path := filepath.Join(book, name)
				data, _ := os.ReadFile(path)
				data = bytes.Replace(data, []byte(`"第01章旧场景01（原文逐字保留）"`), []byte(`""`), 1)
				mustWrite(t, path, data)
			}
		},
		"incomplete-structured-v2": func(t *testing.T, book string) {
			for _, name := range []string{"outline.json", "layered_outline.json"} {
				path := filepath.Join(book, name)
				data, _ := os.ReadFile(path)
				data = bytes.Replace(data, []byte("\"goal\": \"目标-35-01\",\n"), nil, 1)
				mustWrite(t, path, data)
			}
		},
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			book, expected := makeFixtureBook(t)
			mutate(t, book)
			gen := &mutatingGenerator{mutate: func(_ ChapterRequest, raw []byte) []byte { return raw }}
			if _, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: gen, ExpectedEnrolled: &expected}); err == nil {
				t.Fatal("invalid source was accepted")
			}
			if gen.calls != 0 {
				t.Fatalf("generator called %d times for invalid source", gen.calls)
			}
		})
	}
}

func TestSourceWhitelistRejectsReparseComponent(t *testing.T) {
	book, expected := makeFixtureBook(t)
	chapters := filepath.Join(book, "chapters")
	target := filepath.Join(t.TempDir(), "chapter-target")
	if err := os.Rename(chapters, target); err != nil {
		t.Fatal(err)
	}
	if err := createDirectoryLink(chapters, target); err != nil {
		t.Skipf("directory reparse creation unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(chapters) })
	gen := &mutatingGenerator{mutate: func(_ ChapterRequest, raw []byte) []byte { return raw }}
	if _, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: gen, ExpectedEnrolled: &expected}); err == nil {
		t.Fatal("draft accepted a reparse component in the source whitelist")
	}
	if gen.calls != 0 {
		t.Fatalf("generator called %d times after source reparse rejection", gen.calls)
	}
}

func TestDraftFailureCancellationCostAndStaleBoundaries(t *testing.T) {
	t.Run("generator-failure", func(t *testing.T) {
		book, expected := makeFixtureBook(t)
		before := mustSnapshot(t, book)
		gen := &errorGenerator{}
		result, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: gen, MaxAttempts: 2, ExpectedEnrolled: &expected})
		if err == nil || gen.calls != 2 {
			t.Fatalf("err=%v calls=%d", err, gen.calls)
		}
		assertNoManifest(t, result.RunDir)
		assertSnapshot(t, book, before)
	})
	t.Run("cancelled", func(t *testing.T) {
		book, expected := makeFixtureBook(t)
		before := mustSnapshot(t, book)
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		result, err := Draft(ctx, DraftOptions{BookDir: book, Generator: FakeGenerator{}, ExpectedEnrolled: &expected})
		if !errors.Is(err, context.Canceled) {
			t.Fatalf("got %v, want context cancellation", err)
		}
		assertNoManifest(t, result.RunDir)
		assertSnapshot(t, book, before)
	})
	t.Run("reservation-stops-before-call", func(t *testing.T) {
		book, expected := makeFixtureBook(t)
		before := mustSnapshot(t, book)
		gen := &pricedGenerator{pricing: Pricing{Known: true, OutputUSDPerMillion: 1_000_000, MaxOutputTokens: 1}}
		result, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: gen, MaxCostUSD: .5, ExpectedEnrolled: &expected})
		if err == nil || gen.calls != 0 {
			t.Fatalf("err=%v calls=%d", err, gen.calls)
		}
		assertNoManifest(t, result.RunDir)
		assertSnapshot(t, book, before)
	})
	t.Run("unknown-pricing-stops-before-call", func(t *testing.T) {
		book, expected := makeFixtureBook(t)
		gen := &pricedGenerator{}
		_, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: gen, ExpectedEnrolled: &expected})
		if err == nil || gen.calls != 0 {
			t.Fatalf("err=%v calls=%d", err, gen.calls)
		}
	})
	t.Run("unknown-usage", func(t *testing.T) {
		book, expected := makeFixtureBook(t)
		before := mustSnapshot(t, book)
		gen := &pricedGenerator{pricing: Pricing{Known: true, OutputUSDPerMillion: 1, MaxOutputTokens: 100}, usage: Usage{Known: false, InputTokens: -1}}
		result, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: gen, ExpectedEnrolled: &expected})
		if err == nil || gen.calls != 1 {
			t.Fatalf("err=%v calls=%d", err, gen.calls)
		}
		var exchange providerExchange
		if readErr := readStrictFile(filepath.Join(result.RunDir, "provider", "exchanges", "ch-01-attempt-01.json"), &exchange); readErr != nil {
			t.Fatal(readErr)
		}
		if exchange.Record.UsageKnown || exchange.Record.ErrorCategory != "usage_error" || exchange.Record.ReservedUSD <= 0 {
			t.Fatalf("unknown-usage attempt receipt is incomplete: %+v", exchange.Record)
		}
		assertNoManifest(t, result.RunDir)
		assertSnapshot(t, book, before)
	})
	t.Run("input-upper-bound-exceeded", func(t *testing.T) {
		book, expected := makeFixtureBook(t)
		before := mustSnapshot(t, book)
		gen := &pricedGenerator{pricing: Pricing{Known: true, MaxOutputTokens: 100}, usage: Usage{Known: true, InputTokens: 2}, inputBound: 1}
		result, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: gen, ExpectedEnrolled: &expected})
		if err == nil || gen.calls != 1 || !strings.Contains(err.Error(), "input token upper bound exceeded") {
			t.Fatalf("err=%v calls=%d", err, gen.calls)
		}
		assertNoManifest(t, result.RunDir)
		assertSnapshot(t, book, before)
	})
	t.Run("stale-source", func(t *testing.T) {
		book, expected := makeFixtureBook(t)
		before := mustSnapshot(t, book)
		premise := filepath.Join(book, "premise.md")
		original, _ := os.ReadFile(premise)
		result, err := Draft(context.Background(), DraftOptions{
			BookDir: book, Generator: FakeGenerator{}, ExpectedEnrolled: &expected,
			AfterBatch: func(chapter int) {
				if chapter == 1 {
					_ = os.WriteFile(premise, append(original, []byte("stale")...), 0o644)
				}
			},
		})
		if err == nil || !strings.Contains(err.Error(), "stale") {
			t.Fatalf("got %v, want stale error", err)
		}
		assertNoManifest(t, result.RunDir)
		mustWrite(t, premise, original)
		assertSnapshot(t, book, before)
	})
}

func TestDraftRetryAccounting(t *testing.T) {
	book, expected := makeFixtureBook(t)
	gen := &retryGenerator{}
	result, err := Draft(context.Background(), DraftOptions{
		BookDir: book, Generator: gen, MaxAttempts: 2, MaxCostUSD: 20, ExpectedEnrolled: &expected,
	})
	if err != nil {
		t.Fatal(err)
	}
	if gen.calls != 84 {
		t.Fatalf("calls = %d, want 84", gen.calls)
	}
	var manifest ArtifactManifest
	if err := readStrictFile(filepath.Join(result.RunDir, "manifest.json"), &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.Attempts != 84 || manifest.CostUSD <= 0 || manifest.CostUSD > 20 {
		t.Fatalf("bad retry accounting: %+v", manifest)
	}
}

func TestVerifyRejectsArtifactTampering(t *testing.T) {
	book, expected := makeFixtureBook(t)
	result, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: FakeGenerator{}, ExpectedEnrolled: &expected})
	if err != nil {
		t.Fatal(err)
	}
	paths := []string{
		"source_snapshot/premise.md",
		"proposals/ch-01.json",
		"provider/records.json",
		"audit.json",
		"review.diff.json",
		"manifest.json",
		"manifest.sha256",
	}
	for _, rel := range paths {
		t.Run(strings.ReplaceAll(rel, "/", "_"), func(t *testing.T) {
			path := filepath.Join(result.RunDir, filepath.FromSlash(rel))
			original, readErr := os.ReadFile(path)
			if readErr != nil {
				t.Fatal(readErr)
			}
			if err := os.WriteFile(path, append(append([]byte(nil), original...), byte('x')), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := Verify(VerifyOptions{BookDir: book, RunDir: result.RunDir, ExpectedEnrolled: &expected}); err == nil {
				t.Fatal("verify accepted tampered artifact")
			}
			if err := os.WriteFile(path, original, 0o644); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestVerifyRejectsResignedProtocolForgery(t *testing.T) {
	tests := map[string]func(*testing.T, string, *ArtifactManifest){
		"extra-empty-directory": func(t *testing.T, run string, _ *ArtifactManifest) {
			if err := os.Mkdir(filepath.Join(run, "unapproved-empty"), 0o755); err != nil {
				t.Fatal(err)
			}
		},
		"extra-artifact": func(t *testing.T, run string, manifest *ArtifactManifest) {
			rel := "unapproved/extra.txt"
			mustWrite(t, filepath.Join(run, filepath.FromSlash(rel)), []byte("not in protocol"))
			manifest.Artifacts = append(manifest.Artifacts, FileHash{Path: rel})
		},
		"omitted-required-artifact": func(t *testing.T, run string, manifest *ArtifactManifest) {
			if err := os.Remove(filepath.Join(run, "audit.json")); err != nil {
				t.Fatal(err)
			}
			manifest.Artifacts = removeArtifact(manifest.Artifacts, "audit.json")
		},
		"forged-cost-accounting": func(t *testing.T, run string, manifest *ArtifactManifest) {
			var records []ProviderAttempt
			if err := readStrictFile(filepath.Join(run, "provider", "records.json"), &records); err != nil {
				t.Fatal(err)
			}
			records[0].CostUSD = 1
			data, _ := marshalJSON(records)
			mustWrite(t, filepath.Join(run, "provider", "records.json"), data)
			manifest.CostUSD = 1
		},
		"forged-review": func(t *testing.T, run string, _ *ArtifactManifest) {
			mustWrite(t, filepath.Join(run, "REVIEW.md"), []byte("attacker supplied review\n"))
		},
	}
	for name, forge := range tests {
		t.Run(name, func(t *testing.T) {
			book, expected := makeFixtureBook(t)
			result, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: FakeGenerator{}, ExpectedEnrolled: &expected})
			if err != nil {
				t.Fatal(err)
			}
			var manifest ArtifactManifest
			if err := readStrictFile(filepath.Join(result.RunDir, "manifest.json"), &manifest); err != nil {
				t.Fatal(err)
			}
			forge(t, result.RunDir, &manifest)
			resignManifest(t, result.RunDir, &manifest)
			if _, err := Verify(VerifyOptions{BookDir: book, RunDir: result.RunDir, ExpectedEnrolled: &expected}); err == nil {
				t.Fatal("verify accepted a re-signed protocol forgery")
			}
		})
	}
}

func removeArtifact(files []FileHash, path string) []FileHash {
	out := files[:0]
	for _, file := range files {
		if file.Path != path {
			out = append(out, file)
		}
	}
	return out
}

func resignManifest(t *testing.T, run string, manifest *ArtifactManifest) {
	t.Helper()
	for i := range manifest.Artifacts {
		file, err := hashFile(filepath.Join(run, filepath.FromSlash(manifest.Artifacts[i].Path)))
		if err != nil {
			t.Fatal(err)
		}
		file.Path = manifest.Artifacts[i].Path
		manifest.Artifacts[i] = file
	}
	sort.Slice(manifest.Artifacts, func(i, j int) bool { return manifest.Artifacts[i].Path < manifest.Artifacts[j].Path })
	data, err := marshalJSON(manifest)
	if err != nil {
		t.Fatal(err)
	}
	mustWrite(t, filepath.Join(run, "manifest.json"), data)
	mustWrite(t, filepath.Join(run, "manifest.sha256"), []byte(hashBytes(data)+"  manifest.json\n"))
}

func TestExclusiveRunLocationAndAtomicCreate(t *testing.T) {
	book, expected := makeFixtureBook(t)
	opts := DraftOptions{BookDir: book, Generator: FakeGenerator{}, ExpectedEnrolled: &expected, Now: fixedNow, RandomBytes: fixedRandom}
	first, err := Draft(context.Background(), opts)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := Draft(context.Background(), opts); err == nil || !strings.Contains(err.Error(), "exclusive") {
		t.Fatalf("collision was not rejected: %v", err)
	}
	outside := t.TempDir()
	if _, err := Verify(VerifyOptions{BookDir: book, RunDir: outside, ExpectedEnrolled: &expected}); err == nil {
		t.Fatal("verify accepted run outside sibling migrations directory")
	}
	path := filepath.Join(t.TempDir(), "atomic.txt")
	if err := atomicCreate(path, []byte("first")); err != nil {
		t.Fatal(err)
	}
	if err := atomicCreate(path, []byte("second")); err == nil {
		t.Fatal("atomicCreate overwrote an existing artifact")
	}
	data, _ := os.ReadFile(path)
	if string(data) != "first" {
		t.Fatalf("existing bytes changed: %q", data)
	}
	if _, err := Verify(VerifyOptions{BookDir: book, RunDir: first.RunDir, ExpectedEnrolled: &expected}); err != nil {
		t.Fatal(err)
	}
}

func TestDraftRejectsMigrationsJunctionBeforeCreatingRun(t *testing.T) {
	book, expected := makeFixtureBook(t)
	bookBefore := mustSnapshot(t, book)
	target := t.TempDir()
	targetBefore := mustSnapshot(t, target)
	migrations := filepath.Join(filepath.Dir(book), "migrations")
	if err := createDirectoryLink(migrations, target); err != nil {
		t.Skipf("directory reparse creation unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(migrations) })
	gen := &mutatingGenerator{mutate: func(_ ChapterRequest, raw []byte) []byte { return raw }}
	result, err := Draft(context.Background(), DraftOptions{BookDir: book, Generator: gen, ExpectedEnrolled: &expected})
	if err == nil {
		t.Fatal("draft accepted migrations junction")
	}
	if result.RunDir != "" || gen.calls != 0 {
		t.Fatalf("junction rejection crossed run/generator boundary: run=%q calls=%d", result.RunDir, gen.calls)
	}
	assertSnapshot(t, book, bookBefore)
	assertSnapshot(t, target, targetBefore)
}

func TestArtifactWriterRejectsNestedJunction(t *testing.T) {
	root := t.TempDir()
	target := t.TempDir()
	link := filepath.Join(root, "proposals")
	if err := createDirectoryLink(link, target); err != nil {
		t.Skipf("directory reparse creation unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if err := atomicCreateArtifact(root, "proposals/ch-01.json", []byte("forbidden")); err == nil {
		t.Fatal("artifact writer followed nested junction")
	}
	if _, err := os.Stat(filepath.Join(target, "ch-01.json")); !os.IsNotExist(err) {
		t.Fatalf("junction target was written: %v", err)
	}
}

func TestVerifyRejectsSymlinkReparseRunEscape(t *testing.T) {
	book, expected := makeFixtureBook(t)
	migrations := filepath.Join(filepath.Dir(book), "migrations")
	if err := os.MkdirAll(migrations, 0o755); err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	link := filepath.Join(migrations, "scene-beat-v3-link")
	if err := createDirectoryLink(link, target); err != nil {
		t.Skipf("directory reparse creation unavailable: %v", err)
	}
	t.Cleanup(func() { _ = os.Remove(link) })
	if _, err := Verify(VerifyOptions{BookDir: book, RunDir: link, ExpectedEnrolled: &expected}); err == nil {
		t.Fatal("verify accepted symlink/reparse run escape")
	}
}

func createDirectoryLink(link, target string) error {
	if err := os.Symlink(target, link); err == nil {
		return nil
	} else if runtime.GOOS != "windows" {
		return err
	}
	output, err := exec.Command("cmd", "/c", "mklink", "/J", link, target).CombinedOutput()
	if err != nil {
		return fmt.Errorf("mklink /J: %w (%s)", err, output)
	}
	return nil
}

type mutatingGenerator struct {
	calls  int
	mutate func(ChapterRequest, []byte) []byte
}

func (g *mutatingGenerator) Descriptor() GeneratorDescriptor { return FakeGenerator{}.Descriptor() }
func (g *mutatingGenerator) Prepare(req ChapterRequest) (PreparedRequest, error) {
	return (FakeGenerator{}).Prepare(req)
}
func (g *mutatingGenerator) Generate(ctx context.Context, req ChapterRequest, prepared PreparedRequest, attempt int) (GenerateResult, error) {
	g.calls++
	result, err := (FakeGenerator{}).Generate(ctx, req, prepared, attempt)
	result.RawResponse = g.mutate(req, result.RawResponse)
	return result, err
}

type errorGenerator struct{ calls int }

func (g *errorGenerator) Descriptor() GeneratorDescriptor { return FakeGenerator{}.Descriptor() }
func (g *errorGenerator) Prepare(req ChapterRequest) (PreparedRequest, error) {
	return (FakeGenerator{}).Prepare(req)
}
func (g *errorGenerator) Generate(context.Context, ChapterRequest, PreparedRequest, int) (GenerateResult, error) {
	g.calls++
	return GenerateResult{Usage: Usage{Known: true}}, errors.New("injected generator failure")
}

type pricedGenerator struct {
	calls      int
	pricing    Pricing
	usage      Usage
	inputBound int
}

func (g *pricedGenerator) Descriptor() GeneratorDescriptor {
	return GeneratorDescriptor{Provider: "test", Model: "test", Pricing: g.pricing}
}
func (g *pricedGenerator) Prepare(req ChapterRequest) (PreparedRequest, error) {
	if g.inputBound > 0 {
		return PreparedRequest{OutboundBody: strings.Repeat("x", g.inputBound)}, nil
	}
	return (FakeGenerator{}).Prepare(req)
}
func (g *pricedGenerator) Generate(ctx context.Context, req ChapterRequest, prepared PreparedRequest, attempt int) (GenerateResult, error) {
	g.calls++
	result, err := (FakeGenerator{}).Generate(ctx, req, prepared, attempt)
	result.Usage = g.usage
	return result, err
}

type retryGenerator struct{ calls int }

func (g *retryGenerator) Descriptor() GeneratorDescriptor {
	return GeneratorDescriptor{Provider: "test", Model: "retry", Pricing: Pricing{Known: true, InputUSDPerMillion: 1, OutputUSDPerMillion: 1, MaxOutputTokens: 100}}
}
func (g *retryGenerator) Prepare(req ChapterRequest) (PreparedRequest, error) {
	return (FakeGenerator{}).Prepare(req)
}
func (g *retryGenerator) Generate(ctx context.Context, req ChapterRequest, prepared PreparedRequest, attempt int) (GenerateResult, error) {
	g.calls++
	if attempt == 1 {
		return GenerateResult{RawResponse: []byte(`{"items":[]}`), Usage: Usage{Known: true, InputTokens: 10, OutputTokens: 10}}, nil
	}
	result, err := (FakeGenerator{}).Generate(ctx, req, prepared, attempt)
	result.Usage = Usage{Known: true, InputTokens: 10, OutputTokens: 20}
	return result, err
}

func makeFixtureBook(t *testing.T) (string, projectprofile.Fingerprint) {
	t.Helper()
	book := filepath.Join(t.TempDir(), "output", "novel")
	if err := os.MkdirAll(filepath.Join(book, "chapters"), 0o755); err != nil {
		t.Fatal(err)
	}
	chapters := make([]domain.OutlineEntry, 0, 42)
	for chapter := 1; chapter <= 42; chapter++ {
		count := 3
		if chapter <= 21 {
			count = 5
		} else if chapter <= 34 {
			count = 4
		} else if chapter == 42 {
			count = 4
		}
		rawScenes := make([]any, 0, count)
		for scene := 1; scene <= count; scene++ {
			if chapter <= 34 {
				rawScenes = append(rawScenes, fmt.Sprintf("第%02d章旧场景%02d（原文逐字保留）", chapter, scene))
			} else {
				rawScenes = append(rawScenes, map[string]string{
					"goal": fmt.Sprintf("目标-%02d-%02d", chapter, scene), "action": "既有动作",
					"conflict": "既有冲突", "outcome": "既有结果", "sensory_anchor": "既有感官锚点",
				})
			}
		}
		raw, _ := json.Marshal(rawScenes)
		var scenes domain.SceneList
		if err := json.Unmarshal(raw, &scenes); err != nil {
			t.Fatal(err)
		}
		chapters = append(chapters, domain.OutlineEntry{
			Chapter: chapter, Title: fmt.Sprintf("第%02d章", chapter), CoreEvent: "核心事件", Hook: "章末钩子", Scenes: scenes,
		})
	}
	volumes := []domain.VolumeOutline{
		{Index: 1, Title: "第一卷", Theme: "卷主题一", Arcs: []domain.ArcOutline{
			{Index: 1, Title: "展开弧一", Goal: "目标一", Chapters: chapters[:34]},
			{Index: 2, Title: "骨架弧一", Goal: "骨架目标一", EstimatedChapters: 10, Chapters: nil},
		}},
		{Index: 2, Title: "第二卷", Theme: "卷主题二", Final: true, Arcs: []domain.ArcOutline{
			{Index: 1, Title: "展开弧二", Goal: "目标二", Chapters: chapters[34:]},
			{Index: 2, Title: "骨架弧二", Goal: "骨架目标二", EstimatedChapters: 12, Chapters: nil},
		}},
	}
	outlineData, _ := marshalJSON(chapters)
	layeredData, _ := marshalJSON(volumes)
	mustWrite(t, filepath.Join(book, "premise.md"), []byte("# 书名：测试迁移书库\n\n严格白名单测试。\n"))
	mustWrite(t, filepath.Join(book, "outline.json"), outlineData)
	mustWrite(t, filepath.Join(book, "outline.md"), []byte(renderOutline(chapters)))
	mustWrite(t, filepath.Join(book, "layered_outline.json"), layeredData)
	mustWrite(t, filepath.Join(book, "layered_outline.md"), []byte(renderLayeredOutline(volumes)))
	for chapter := 1; chapter <= 34; chapter++ {
		mustWrite(t, filepath.Join(book, "chapters", fmt.Sprintf("%02d.md", chapter)), []byte(fmt.Sprintf("# 第%02d章\n\n正文原始字节。\n", chapter)))
	}
	fp, err := projectprofile.NewStoreFingerprinter(book)()
	if err != nil {
		t.Fatal(err)
	}
	return book, fp
}

func mustWrite(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func mustSnapshot(t *testing.T, root string) map[string]string {
	t.Helper()
	snapshot, err := snapshotTree(root)
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

func assertSnapshot(t *testing.T, root string, want map[string]string) {
	t.Helper()
	got := mustSnapshot(t, root)
	w, _ := json.Marshal(want)
	g, _ := json.Marshal(got)
	if !bytes.Equal(w, g) {
		t.Fatalf("book snapshot changed\nwant=%s\ngot=%s", w, g)
	}
}

func assertNoManifest(t *testing.T, runDir string) {
	t.Helper()
	if runDir == "" {
		return
	}
	if _, err := os.Stat(filepath.Join(runDir, "manifest.json")); !os.IsNotExist(err) {
		t.Fatalf("incomplete run has final manifest: %v", err)
	}
}

func fixedNow() time.Time { return time.Date(2026, 7, 17, 12, 34, 56, 0, time.UTC) }
func fixedRandom(p []byte) error {
	for i := range p {
		p[i] = byte(i + 1)
	}
	return nil
}
