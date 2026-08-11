// Copyright 2026 The Hugo Authors. All rights reserved.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
// http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package hugolib

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	qt "github.com/frankban/quicktest"
)

const incrementalFilesTemplate = `
-- hugo.toml --
baseURL = "https://example.com"
incremental = true
cacheDir = "WORKING_DIR/hugocache"
disableKinds = ["term", "taxonomy", "sitemap", "robotstxt", "404", "rss"]
-- content/s1/_index.md --
-- content/s1/p1.md --
---
title: P1
---
P1 content.
-- content/s2/p2.md --
---
title: P2
---
P2 content.
-- layouts/page.html --
Page: {{ .Title }}|{{ .Content }}|
-- layouts/list.html --
List: {{ .Title }}|{{ range .RegularPagesRecursive }}{{ .RelPermalink }}|{{ end }}$
`

var incrementalFilesWithP3 = strings.ReplaceAll(incrementalFilesTemplate, "-- content/s1/_index.md --", `-- content/s1/p3.md --
---
title: P3
---
P3 content.
-- content/s1/_index.md --`)

func buildIncremental(t *testing.T, workingDir, files string) *IntegrationTestBuilder {
	t.Helper()
	files = strings.ReplaceAll(files, "WORKING_DIR", filepath.ToSlash(workingDir))
	return Test(t, files, TestOptWithConfig(func(cfg *IntegrationTestConfig) {
		cfg.NeedsOsFS = true
		cfg.WorkingDir = workingDir
	}))
}

func TestIncrementalBuildNoChange(t *testing.T) {
	workingDir := t.TempDir()

	b := buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertRenderCountPage(5) // home, s1, s1/p1, s2, s2/p2
	b.Assert(fileExists(filepath.Join(workingDir, "hugocache", buildStateFilename)), qt.IsTrue)

	b = buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertRenderCountPage(0)
	b.AssertFileContent("public/index.html", "/s1/p1/|/s2/p2/|$")
}

func TestIncrementalBuildEditContent(t *testing.T) {
	workingDir := t.TempDir()

	b := buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertRenderCountPage(5)

	files := strings.ReplaceAll(incrementalFilesTemplate, "P1 content.", "P1 content edited.")
	b = buildIncremental(t, workingDir, files)
	b.AssertFileContent("public/s1/p1/index.html", "P1 content edited.")
	// The edited page and its dependents (s1 and home list it).
	b.AssertRenderCountPage(3)
}

func TestIncrementalBuildAddPage(t *testing.T) {
	workingDir := t.TempDir()

	b := buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertRenderCountPage(5)

	b = buildIncremental(t, workingDir, incrementalFilesWithP3)
	b.AssertFileContent("public/s1/index.html", "/s1/p1/|/s1/p3/|$")
	b.AssertFileContent("public/index.html", "/s1/p3/|")
	// s2 and s2/p2 must not re-render.
	b.AssertFileContent("public/s2/p2/index.html", "P2 content.")
	b.AssertRenderCountPage(4) // p3, s1, home + sampled sibling p1.
}

func TestIncrementalBuildRemovePage(t *testing.T) {
	workingDir := t.TempDir()

	b := buildIncremental(t, workingDir, incrementalFilesWithP3)
	b.AssertRenderCountPage(6)
	b.AssertFileContent("public/s1/index.html", "/s1/p1/|/s1/p3/|$")

	b.Assert(os.Remove(filepath.Join(workingDir, "content", "s1", "p3.md")), qt.IsNil)
	b = buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertFileContent("public/s1/index.html", "/s1/p1/|$")
	b.AssertFileContent("public/index.html", "! /s1/p3/")
}

func TestIncrementalBuildEditTemplate(t *testing.T) {
	workingDir := t.TempDir()

	b := buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertRenderCountPage(5)

	files := strings.ReplaceAll(incrementalFilesTemplate, "Page: {{ .Title }}", "Page!: {{ .Title }}")
	b = buildIncremental(t, workingDir, files)
	b.AssertFileContent("public/s1/p1/index.html", "Page!: P1")
	b.AssertFileContent("public/s2/p2/index.html", "Page!: P2")
	// Only the pages using page.html.
	b.AssertRenderCountPage(2)
}

func TestIncrementalBuildMissingTarget(t *testing.T) {
	workingDir := t.TempDir()

	b := buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertRenderCountPage(5)

	b.Assert(os.Remove(filepath.Join(workingDir, "public", "s1", "p1", "index.html")), qt.IsNil)

	b = buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertRenderCountPage(1)
	b.AssertFileContent("public/s1/p1/index.html", "P1 content.")
}

func TestIncrementalBuildDisabled(t *testing.T) {
	workingDir := t.TempDir()
	files := strings.ReplaceAll(incrementalFilesTemplate, "incremental = true", "")

	b := buildIncremental(t, workingDir, files)
	b.AssertRenderCountPage(5)
	b.Assert(fileExists(filepath.Join(workingDir, "hugocache", buildStateFilename)), qt.IsFalse)

	b = buildIncremental(t, workingDir, files)
	b.AssertRenderCountPage(5)
}

func fileExists(filename string) bool {
	_, err := os.Stat(filename)
	return err == nil
}

func TestIncrementalBuildEditConfig(t *testing.T) {
	workingDir := t.TempDir()

	b := buildIncremental(t, workingDir, incrementalFilesTemplate)
	b.AssertRenderCountPage(5)

	files := strings.ReplaceAll(incrementalFilesTemplate, `baseURL = "https://example.com"`, `baseURL = "https://example.org"`)
	b = buildIncremental(t, workingDir, files)
	// Config changed, full build.
	b.AssertRenderCountPage(5)
}

func TestIncrementalBuildEditAssetAndData(t *testing.T) {
	files := strings.ReplaceAll(incrementalFilesTemplate, "-- content/s1/_index.md --", `-- assets/main.css --
body { color: red; }
-- data/mydata.toml --
greeting = "hello"
-- content/s1/_index.md --`)
	files = strings.ReplaceAll(files, "-- layouts/page.html --\nPage: {{ .Title }}|{{ .Content }}|",
		`-- layouts/page.html --
Page: {{ .Title }}|{{ .Content }}|{{ with resources.Get "main.css" }}{{ .RelPermalink }}{{ end }}|{{ site.Data.mydata.greeting }}|`)

	workingDir := t.TempDir()
	b := buildIncremental(t, workingDir, files)
	b.AssertRenderCountPage(5)
	b.AssertFileContent("public/s1/p1/index.html", "hello")

	edited := strings.ReplaceAll(files, "color: red", "color: blue")
	b = buildIncremental(t, workingDir, edited)
	// Only the pages using the asset.
	b.AssertRenderCountPage(2)

	edited = strings.ReplaceAll(files, `greeting = "hello"`, `greeting = "hi"`)
	b = buildIncremental(t, workingDir, edited)
	b.AssertFileContent("public/s1/p1/index.html", "hi")
	b.AssertRenderCountPage(2)
}
