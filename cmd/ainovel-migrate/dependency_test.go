package main

import (
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
)

func TestStandaloneSourceImportsExcludeApplicationRuntime(t *testing.T) {
	forbidden := []string{
		"/internal/host", "/internal/store", "/internal/runmeta", "/internal/engine",
		"/internal/workers", "/internal/flow",
	}
	roots := []string{".", filepath.Join("..", "..", "internal", "migratev3")}
	for _, root := range roots {
		entries, err := os.ReadDir(root)
		if err != nil {
			t.Fatal(err)
		}
		for _, entry := range entries {
			if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
				continue
			}
			path := filepath.Join(root, entry.Name())
			file, err := parser.ParseFile(token.NewFileSet(), path, nil, parser.ImportsOnly)
			if err != nil {
				t.Fatal(err)
			}
			for _, spec := range file.Imports {
				importPath, _ := strconv.Unquote(spec.Path.Value)
				for _, blocked := range forbidden {
					if strings.Contains(importPath, blocked) {
						t.Fatalf("standalone source %s imports forbidden runtime package %s", path, importPath)
					}
				}
			}
		}
	}
}
