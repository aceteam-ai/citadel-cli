package rules

import (
	"encoding/base64"
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

type artifactVector struct {
	ID           string `json:"id"`
	Operation    string `json:"operation"`
	RefreshOuter bool   `json:"refresh_outer"`
	Accept       bool   `json:"accept"`
	Reason       string `json:"reason"`
}

type artifactVectorFile struct {
	SchemaVersion           string           `json:"schema_version"`
	FixtureCommit           string           `json:"fixture_commit"`
	IndexFile               string           `json:"index_file"`
	IndexDigest             string           `json:"index_digest"`
	ID                      string           `json:"id"`
	Version                 string           `json:"version"`
	PackDigest              string           `json:"pack_digest"`
	BundleFile              string           `json:"bundle_file"`
	BundleDigest            string           `json:"bundle_digest"`
	CanonicalManifestBase64 string           `json:"canonical_manifest_base64"`
	Cases                   []artifactVector `json:"cases"`
	JSONSchemaCases         []struct {
		ID     string `json:"id"`
		Target string `json:"target"`
		Old    string `json:"old"`
		New    string `json:"new"`
		Accept bool   `json:"accept"`
	} `json:"json_schema_cases"`
}

func artifactTestInputs(t *testing.T) (artifactVectorFile, []byte, []byte) {
	t.Helper()
	root := filepath.Join("..", "..", "..")
	data, err := os.ReadFile(filepath.Join(root, "packs", "vectors", "v1.json"))
	if err != nil {
		t.Fatal(err)
	}
	var vectors artifactVectorFile
	if err := json.Unmarshal(data, &vectors); err != nil {
		t.Fatal(err)
	}
	if vectors.SchemaVersion != "stream.pack-artifact-vectors.v1" || len(vectors.Cases) != 22 || len(vectors.JSONSchemaCases) != 17 {
		t.Fatal("unexpected shared artifact vector contract")
	}
	index, err := os.ReadFile(filepath.Join(root, vectors.IndexFile))
	if err != nil {
		t.Fatal(err)
	}
	bundle, err := os.ReadFile(filepath.Join(root, vectors.BundleFile))
	if err != nil {
		t.Fatal(err)
	}
	if artifactSHA(index) != vectors.IndexDigest || artifactSHA(bundle) != vectors.BundleDigest {
		t.Fatal("shared positive vector byte pins changed")
	}
	return vectors, index, bundle
}

func TestPublishedArtifactAndLocalPackHaveIdenticalCompilerAndGoldenResults(t *testing.T) {
	vectors, indexBytes, bundleBytes := artifactTestInputs(t)
	pin := ArtifactPin{Commit: vectors.FixtureCommit, IndexDigest: vectors.IndexDigest}
	compiled, err := CompilePinnedPack(pin, indexBytes, bundleBytes, vectors.ID, vectors.Version, vectors.PackDigest)
	if err != nil {
		t.Fatal(err)
	}
	local := shieldPack(t)
	if compiled.Digest != local.Digest || compiled.Digest != vectors.PackDigest {
		t.Fatal("artifact and original fixture pack normalization diverged")
	}
	artifactManifest, err := CanonicalManifestBytes(compiled.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	localManifest, err := CanonicalManifestBytes(local.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if string(artifactManifest) != string(localManifest) {
		t.Fatal("normalized artifact manifest differs from local manifest")
	}
	fixtures, expected := shieldFixtures(t)
	for _, mode := range []string{"ingest", "event"} {
		for _, fixture := range fixtures {
			got, actors := shieldEvaluate(t, compiled, fixture, mode)
			want, _ := shieldEvaluate(t, local, fixture, mode)
			if !reflect.DeepEqual(got, want) || !reflect.DeepEqual(shieldProject(t, got, actors), expected[fixture.name]) {
				t.Fatalf("artifact changed shared golden scenario %s/%s", mode, fixture.name)
			}
		}
	}
	var bundle artifactBundle
	if err := decodeArtifactJSON(bundleBytes, &bundle); err != nil {
		t.Fatal(err)
	}
	if bundle.ManifestBase64 != vectors.CanonicalManifestBase64 {
		t.Fatal("pinned canonical manifest bytes changed")
	}
	canonical, err := CanonicalManifestBytes(local.Manifest)
	if err != nil {
		t.Fatal(err)
	}
	if base64.StdEncoding.EncodeToString(canonical) != vectors.CanonicalManifestBase64 {
		t.Fatal("Go struct field order or omitempty canonicalization changed")
	}
	for _, rule := range bundle.Rules {
		original, err := os.ReadFile(filepath.Join("..", "testdata", rule.File))
		if err != nil {
			t.Fatal(err)
		}
		if base64.StdEncoding.EncodeToString(original) != rule.SourceBase64 {
			t.Fatalf("artifact does not preserve exact original rule bytes: %s", rule.File)
		}
	}
}

func TestSharedArtifactTamperVectors(t *testing.T) {
	vectors, originalIndex, originalBundle := artifactTestInputs(t)
	for _, vector := range vectors.Cases {
		t.Run(vector.ID, func(t *testing.T) {
			pin := ArtifactPin{Commit: vectors.FixtureCommit, IndexDigest: vectors.IndexDigest}
			indexBytes, bundleBytes := append([]byte(nil), originalIndex...), append([]byte(nil), originalBundle...)
			selectorDigest := vectors.PackDigest
			var index PackIndex
			var bundle artifactBundle
			if err := decodeArtifactJSON(indexBytes, &index); err != nil {
				t.Fatal(err)
			}
			if err := decodeArtifactJSON(bundleBytes, &bundle); err != nil {
				t.Fatal(err)
			}
			bundleChanged := false
			switch vector.Operation {
			case "none":
			case "index_append_space":
				indexBytes = append(indexBytes, ' ')
			case "bundle_append_space":
				bundleBytes = append(bundleBytes, ' ')
			case "rule_append_newline":
				data, err := artifactDecodeBase64(bundle.Rules[0].SourceBase64)
				if err != nil {
					t.Fatal(err)
				}
				bundle.Rules[0].SourceBase64 = base64.StdEncoding.EncodeToString(append(data, '\n'))
				bundleChanged = true
			case "remove_last_rule":
				bundle.Rules = bundle.Rules[:len(bundle.Rules)-1]
				bundleChanged = true
			case "append_unknown_rule":
				bundle.Rules = append(bundle.Rules, artifactRule{File: "rules/unknown.yml", SourceBase64: bundle.Rules[0].SourceBase64})
				bundleChanged = true
			case "swap_first_rules":
				bundle.Rules[0], bundle.Rules[1] = bundle.Rules[1], bundle.Rules[0]
				bundleChanged = true
			case "duplicate_first_file":
				bundle.Rules[1].File = bundle.Rules[0].File
				bundleChanged = true
			case "manifest_append_space", "manifest_sort_keys", "manifest_empty_field_map":
				data, err := artifactDecodeBase64(bundle.ManifestBase64)
				if err != nil {
					t.Fatal(err)
				}
				switch vector.Operation {
				case "manifest_append_space":
					data = append(data, ' ')
				case "manifest_sort_keys":
					var object map[string]any
					if err := json.Unmarshal(data, &object); err != nil {
						t.Fatal(err)
					}
					data, err = json.Marshal(object)
					if err != nil {
						t.Fatal(err)
					}
				case "manifest_empty_field_map":
					data = []byte(strings.Replace(string(data), `,"enrichments":`, `,"field_map":{},"enrichments":`, 1))
				}
				bundle.ManifestBase64 = base64.StdEncoding.EncodeToString(data)
				bundleChanged = true
			case "rule_base64_newline":
				bundle.Rules[0].SourceBase64 += "\n"
				bundleChanged = true
			case "unknown_bundle_field":
				var object map[string]any
				if err := json.Unmarshal(bundleBytes, &object); err != nil {
					t.Fatal(err)
				}
				object["unknown"] = true
				var err error
				bundleBytes, err = json.Marshal(object)
				if err != nil {
					t.Fatal(err)
				}
			case "duplicate_index_origin":
			case "index_path_escape":
				index.Packs[0].BundlePath = "../bundle.json"
			case "non_commit_pin":
				pin.Commit = "main"
			case "unknown_pack_digest":
				selectorDigest = "sha256:" + strings.Repeat("0", 64)
			case "index_wrong_origin":
				index.Origin = "attacker/repository"
			case "index_future_schema":
				index.SchemaVersion = "stream.pack-index.v2"
			case "index_duplicate_entry":
				index.Packs = append(index.Packs, index.Packs[0])
			case "index_uppercase_digest":
				index.Packs[0].Digest = strings.ToUpper(index.Packs[0].Digest)
			case "index_url_path":
				index.Packs[0].BundlePath = "https://example.test/bundle.json"
			default:
				t.Fatalf("unknown shared mutation operation: %s", vector.Operation)
			}
			if bundleChanged {
				var err error
				bundleBytes, err = json.Marshal(bundle)
				if err != nil {
					t.Fatal(err)
				}
			}
			if vector.RefreshOuter {
				index.Packs[0].BundleSHA256 = artifactSHA(bundleBytes)
				var err error
				indexBytes, err = json.Marshal(index)
				if err != nil {
					t.Fatal(err)
				}
				if vector.Operation == "duplicate_index_origin" {
					indexBytes = []byte(strings.Replace(string(indexBytes), `"origin":`, `"origin":"aceteam-ai/citadel-cli","origin":`, 1))
				}
				pin.IndexDigest = artifactSHA(indexBytes)
			}
			compiled, err := CompilePinnedPack(pin, indexBytes, bundleBytes, vectors.ID, vectors.Version, selectorDigest)
			if vector.Accept {
				if err != nil || compiled == nil || compiled.Digest != vectors.PackDigest {
					t.Fatalf("positive vector refused: %v", err)
				}
			} else if err == nil || compiled != nil || !strings.Contains(err.Error(), vector.Reason) {
				t.Fatalf("tamper vector accepted or wrong refusal: %v; want %s", err, vector.Reason)
			}
		})
	}
}

func TestArtifactLimitsAndStrictIndexSchema(t *testing.T) {
	vectors, originalIndex, bundle := artifactTestInputs(t)
	for _, value := range []string{"", "YQ", "YR==", "YQ==\n", strings.Repeat("A", (maxRuleBytes+3)/3*4)} {
		if _, err := artifactDecodedSize(value, maxRuleBytes); err == nil {
			if _, err := artifactDecodeBase64(value); err == nil {
				t.Fatalf("invalid base64 accepted: length %d", len(value))
			}
		}
	}
	oversize := make([]byte, maxPackIndexBytes+1)
	if got, err := ParsePinnedPackIndex(ArtifactPin{Commit: vectors.FixtureCommit, IndexDigest: artifactSHA(oversize)}, oversize); err == nil || got != nil {
		t.Fatal("oversize index accepted")
	}
	oversize = make([]byte, maxPackBundleBytes+1)
	if got, err := CompilePinnedPack(ArtifactPin{Commit: vectors.FixtureCommit, IndexDigest: vectors.IndexDigest}, originalIndex, oversize, vectors.ID, vectors.Version, vectors.PackDigest); err == nil || got != nil {
		t.Fatal("oversize bundle accepted")
	}
	if len(bundle) == 0 {
		t.Fatal("missing bounded positive bundle")
	}
}

func TestSharedArtifactJSONSchemaVectors(t *testing.T) {
	vectors, originalIndex, originalBundle := artifactTestInputs(t)
	seen := map[string]bool{}
	for _, vector := range vectors.JSONSchemaCases {
		if vector.ID == "" || seen[vector.ID] {
			t.Fatal("empty or duplicate JSON schema vector id")
		}
		seen[vector.ID] = true
		t.Run(vector.ID, func(t *testing.T) {
			var original []byte
			var decoded any
			switch vector.Target {
			case "index":
				original, decoded = originalIndex, &PackIndex{}
			case "bundle":
				original, decoded = originalBundle, &artifactBundle{}
			default:
				t.Fatal("unknown shared JSON schema target", vector.Target)
			}
			if vector.Old == "" || !strings.Contains(string(original), vector.Old) {
				t.Fatal("shared JSON mutation did not target original bytes")
			}
			data := []byte(strings.Replace(string(original), vector.Old, vector.New, 1))
			if string(data) == string(original) {
				t.Fatal("shared JSON mutation did not change bytes")
			}
			err := decodeArtifactJSON(data, decoded)
			if (err == nil) != vector.Accept {
				t.Fatalf("schema-only acceptance=%v, want %v: %v", err == nil, vector.Accept, err)
			}
		})
	}
}
