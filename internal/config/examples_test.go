package config

import (
	"os"
	"path/filepath"
	"testing"
)

// Every shipped example under examples/tasks.d/ must load and validate
// cleanly — stale documentation is a bug, and samples users copy verbatim
// must never be the thing that breaks their config.
func TestShippedExamplesValidate(t *testing.T) {
	examples, err := filepath.Glob(filepath.Join("..", "..", "examples", "tasks.d", "*.toml"))
	if err != nil {
		t.Fatal(err)
	}
	if len(examples) == 0 {
		t.Fatal("no example files found — did examples/tasks.d/ move?")
	}

	for _, example := range examples {
		t.Run(filepath.Base(example), func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, ConfigFileName), []byte("[daemon]\napi_key = \"example-test\"\n"), 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.Mkdir(filepath.Join(dir, TasksDirName), 0o700); err != nil {
				t.Fatal(err)
			}
			data, err := os.ReadFile(example)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(dir, TasksDirName, filepath.Base(example)), data, 0o600); err != nil {
				t.Fatal(err)
			}

			cfg, err := Load(dir)
			if err != nil {
				t.Fatalf("example does not load: %v", err)
			}
			if len(cfg.Tasks) == 0 {
				t.Fatal("example defines no task")
			}
			if errs := cfg.Validate(os.LookupEnv); len(errs) > 0 {
				t.Fatalf("example does not validate: %v", errs)
			}
		})
	}
}
