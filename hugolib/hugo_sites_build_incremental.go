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
	"compress/gzip"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/gohugoio/hugo/common/hashing"
	"github.com/gohugoio/hugo/common/herrors"
	"github.com/gohugoio/hugo/common/hugo"
	"github.com/gohugoio/hugo/common/paths"
	"github.com/gohugoio/hugo/common/types"
	"github.com/gohugoio/hugo/config"
	"github.com/gohugoio/hugo/hugofs"
	"github.com/gohugoio/hugo/hugofs/files"
	"github.com/gohugoio/hugo/hugofs/hglob"
	"github.com/gohugoio/hugo/hugolib/sitesmatrix"
	"github.com/gohugoio/hugo/identity"
	"github.com/gohugoio/hugo/resources/page/siteidentities"
	"github.com/spf13/afero"
)

// Incremental builds (the incremental setting/--incremental flag) persist a
// build state file across one-shot builds and skip rendering page outputs
// that are unaffected by source changes since the last build.

const (
	buildStateVersion  = 2
	buildStateFilename = "hugo_build_state.json.gz"
)

type buildState struct {
	Version     int
	HugoVersion string
	ConfigHash  string
	PublishDir  string

	// Component -> path -> content hash.
	Files map[string]map[string]string

	// Interned dependency identifier bases.
	Deps []string

	Pages []buildStatePage
}

type buildStatePage struct {
	Site   string
	Path   string
	Format string
	Target string
	Deps   []int
}

func statePageKey(site, path, format string) string {
	return site + "|" + path + "|" + format
}

func siteKey(dims types.Strings3) string {
	return strings.Join(dims[:], ",")
}

type incrementalSkipKey struct {
	site   sitesmatrix.Vector
	path   string
	format string
}

type incrementalBuild struct {
	prev      *buildState
	cur       *buildState
	prevPages map[string]buildStatePage
	// Render decisions, true means skip.
	skip    map[incrementalSkipKey]bool
	skipped int
}

func (h *HugoSites) incrementalEnabled(conf *BuildCfg) bool {
	return h.incrementalEnabledBase() &&
		!conf.SkipRender &&
		!conf.PartialReRender &&
		h.BuildState.BuildCounter.Load() == 0
}

func (h *HugoSites) incrementalEnabledBase() bool {
	c := h.Configs.Base
	return c.Incremental &&
		!c.Internal.Watch &&
		!c.Internal.Running
}

func (h *HugoSites) incrementalPublishDir() string {
	return paths.AbsPathify(h.Conf.WorkingDir(), h.Conf.Dirs().PublishDir)
}

// incrementalInit fingerprints the sources and loads the previous build state.
// It is safe to call from multiple goroutines; the work is done once.
func (h *HugoSites) incrementalInit() *incrementalBuild {
	h.incrementalInitOnce.Do(func() {
		cur, err := h.incrementalFingerprint()
		if err != nil {
			h.Log.Warnf("incremental: failed to fingerprint sources, doing a full build: %s", err)
			return
		}
		ib := &incrementalBuild{cur: cur}
		h.incremental = ib

		prev, err := h.incrementalLoadState()
		if err != nil {
			h.Log.Infof("incremental: %s, doing a full build", err)
			return
		}
		if prev == nil {
			h.Log.Infof("incremental: no previous build state, doing a full build")
			return
		}
		if prev.HugoVersion != cur.HugoVersion || prev.ConfigHash != cur.ConfigHash || prev.PublishDir != cur.PublishDir {
			h.Log.Infof("incremental: Hugo version, configuration or publish directory changed, doing a full build")
			return
		}
		if _, err := hugofs.Os.Stat(cur.PublishDir); err != nil {
			h.Log.Infof("incremental: publish directory missing, doing a full build")
			return
		}
		ib.prev = prev
		ib.prevPages = make(map[string]buildStatePage)
		for _, p := range prev.Pages {
			ib.prevPages[statePageKey(p.Site, p.Path, p.Format)] = p
		}
	})
	return h.incremental
}

func staticComponent(lang string) string {
	if lang == "" {
		return files.ComponentFolderStatic
	}
	return files.ComponentFolderStatic + "/" + lang
}

// IncrementalStaticChanges returns the static files (slash separated, leading slash)
// that need to be synced to the publish dir for the given language and the total
// number of static files. With cleanDestinationDir set, published files whose
// source was removed are deleted. ok is false when a full sync is needed.
func (h *HugoSites) IncrementalStaticChanges(lang string) (changed []string, removed, total int, ok bool) {
	if !h.incrementalEnabledBase() {
		return
	}
	ib := h.incrementalInit()
	if ib == nil || ib.prev == nil {
		return
	}
	c := staticComponent(lang)
	d := diffFingerprints(
		map[string]map[string]string{c: ib.prev.Files[c]},
		map[string]map[string]string{c: ib.cur.Files[c]},
	)
	changed = append(d.changed[c], d.added[c]...)
	sort.Strings(changed)

	if h.Configs.Base.CleanDestinationDir {
		publishFolder := h.BaseFs.SourceFilesystems.Static[lang].PublishFolder
		for _, p := range d.removed[c] {
			if err := removePublished(h.BaseFs.PublishFsStatic, filepath.Join(publishFolder, filepath.FromSlash(p))); err != nil {
				h.Log.Warnf("incremental: failed to remove %q: %s", p, err)
				continue
			}
			removed++
		}
	}

	return changed, removed, len(ib.cur.Files[c]), true
}

// removePublished removes filename and any parent directories left empty.
func removePublished(fs afero.Fs, filename string) error {
	if err := fs.Remove(filename); err != nil && !herrors.IsNotExist(err) {
		return err
	}
	for dir := filepath.Dir(filename); dir != "." && dir != "/" && dir != string(filepath.Separator); dir = filepath.Dir(dir) {
		if fs.Remove(dir) != nil {
			// Not empty.
			break
		}
	}
	return nil
}

// IncrementalDiscardState removes the persisted build state, e.g. after a failed static sync.
func (h *HugoSites) IncrementalDiscardState() {
	if h.incremental == nil {
		return
	}
	if err := hugofs.Os.Remove(h.incrementalStateFilename()); err != nil && !herrors.IsNotExist(err) {
		h.Log.Warnf("incremental: failed to remove build state: %s", err)
	}
}

func (h *HugoSites) incrementalStateFilename() string {
	return filepath.Join(h.Conf.Dirs().CacheDir, buildStateFilename)
}

// incrementalPrepareRender is called after process/assemble and before render.
// It decides which page outputs can be skipped in the render step.
func (h *HugoSites) incrementalPrepareRender(conf *BuildCfg) {
	if !h.incrementalEnabled(conf) {
		return
	}

	ib := h.incrementalInit()
	if ib == nil || ib.prev == nil {
		return
	}
	prev, cur := ib.prev, ib.cur

	changes, dirty, full := h.incrementalChanges(prev, cur)
	if full {
		return
	}

	matcher := newChangeMatcher(changes)
	skip := make(map[incrementalSkipKey]bool)

	h.withPage(func(key string, p *pageState) bool {
		if p.m.isStandalone() {
			// 404, sitemap etc. are cheap; keep them always fresh.
			return false
		}
		if dirty[p.Path()] {
			return false
		}
		if p.initPage() != nil {
			// Let the render step handle the error.
			return false
		}
		sk := siteKey(p.s.resolveDimensionNames())
		for _, po := range p.pageOutputs {
			if !po.render {
				continue
			}
			entry, ok := ib.prevPages[statePageKey(sk, p.Path(), po.f.Name)]
			if !ok {
				// New page output.
				continue
			}
			if entry.Target != "" {
				if _, err := h.BaseFs.PublishFs.Stat(filepath.FromSlash(entry.Target)); err != nil {
					continue
				}
			}
			if matcher.matches(prev.Deps, entry.Deps) {
				continue
			}
			skip[incrementalSkipKey{p.s.siteVector, p.Path(), po.f.Name}] = true
			ib.skipped++
		}
		return false
	})

	ib.skip = skip
	conf.incrementalSkip = skip
	h.Log.Infof("incremental: skipping render of %d page outputs", ib.skipped)
}

type sourceDiff struct {
	// Component -> paths.
	changed map[string][]string
	added   map[string][]string
	removed map[string][]string
}

func diffFingerprints(prev, cur map[string]map[string]string) sourceDiff {
	d := sourceDiff{
		changed: make(map[string][]string),
		added:   make(map[string][]string),
		removed: make(map[string][]string),
	}
	for component, curFiles := range cur {
		prevFiles := prev[component]
		for p, hash := range curFiles {
			prevHash, ok := prevFiles[p]
			switch {
			case !ok:
				d.added[component] = append(d.added[component], p)
			case prevHash != hash:
				d.changed[component] = append(d.changed[component], p)
			}
		}
		for p := range prevFiles {
			if _, ok := curFiles[p]; !ok {
				d.removed[component] = append(d.removed[component], p)
			}
		}
	}
	return d
}

// incrementalChanges maps the source diff to a change set of identities,
// a set of page paths that must be re-rendered, or full = true when the
// change cannot be confidently mapped.
func (h *HugoSites) incrementalChanges(prev, cur *buildState) (changes []identity.Identity, dirty map[string]bool, full bool) {
	d := diffFingerprints(prev.Files, cur.Files)

	logFull := func(why string) {
		h.Log.Infof("incremental: %s, doing a full build", why)
		full = true
	}

	if len(d.changed[files.ComponentFolderI18n])+len(d.added[files.ComponentFolderI18n])+len(d.removed[files.ComponentFolderI18n]) > 0 {
		logFull("translation files changed")
		return
	}

	if len(d.added[files.ComponentFolderLayouts])+len(d.removed[files.ComponentFolderLayouts]) > 0 {
		// Adding or removing a template may change template lookups site wide.
		logFull("templates added or removed")
		return
	}
	for _, p := range d.changed[files.ComponentFolderLayouts] {
		if strings.Contains(p, "_markup") {
			// Render hooks have a hard to determine change set.
			logFull("markup render hooks changed")
			return
		}
		ti := h.GetTemplateStore().GetIdentity(p)
		if ti == nil {
			logFull(fmt.Sprintf("cannot resolve changed template %q", p))
			return
		}
		changes = append(changes, ti)
		if strings.Contains(p, "_shortcodes") {
			pi := h.Conf.PathParser().Parse(files.ComponentFolderLayouts, p)
			changes = append(changes, hglob.NewGlobIdentity(fmt.Sprintf("/_shortcodes/%s*", pi.BaseNameNoIdentifier())))
		}
	}

	if len(d.changed[files.ComponentFolderData])+len(d.added[files.ComponentFolderData])+len(d.removed[files.ComponentFolderData]) > 0 {
		changes = append(changes, siteidentities.Data)
	}

	for _, m := range []map[string][]string{d.changed, d.added, d.removed} {
		for _, p := range m[files.ComponentFolderAssets] {
			changes = append(changes, identity.StringIdentity(p))
		}
	}

	dirty = make(map[string]bool)
	addDirty := func(p string) *paths.Path {
		pi := h.Conf.PathParser().Parse(files.ComponentFolderContent, p)
		dirty[pi.Base()] = true
		if owner, ok := h.pageTrees.treePages.LongestPrefixRaw(pi.Base()); ok {
			dirty[owner] = true
		}
		return pi
	}

	for _, p := range d.changed[files.ComponentFolderContent] {
		if isContentAdapterFile(p) {
			logFull("content adapter file changed")
			return
		}
		pi := addDirty(p)
		changes = append(changes, identity.StringIdentity(pi.Base()))
	}
	for _, p := range d.added[files.ComponentFolderContent] {
		if isContentAdapterFile(p) {
			logFull("content adapter file added")
			return
		}
		pi := addDirty(p)
		changes = append(changes, identity.StructuralChangeAdd)
		changes = append(changes, h.pageTrees.collectIdentitiesSurrounding(pi.Base(), 10)...)
		changes = append(changes, h.pageTrees.collectIdentitiesAncestors(pi.Base())...)
	}
	for _, p := range d.removed[files.ComponentFolderContent] {
		if isContentAdapterFile(p) {
			logFull("content adapter file removed")
			return
		}
		pi := addDirty(p)
		changes = append(changes, identity.StructuralChangeRemove)
		changes = append(changes, identity.StringIdentity(pi.Base()))
		changes = append(changes, h.pageTrees.collectIdentitiesSurrounding(pi.Base(), 10)...)
		changes = append(changes, h.pageTrees.collectIdentitiesAncestors(pi.Base())...)
	}

	return
}

func isContentAdapterFile(p string) bool {
	return strings.HasSuffix(p, "/_content.gotmpl") || p == "/_content.gotmpl"
}

type changeMatcher struct {
	all      bool
	bases    map[string]bool
	probably []identity.IsProbablyDependentProvider
}

func newChangeMatcher(ids []identity.Identity) *changeMatcher {
	m := &changeMatcher{bases: make(map[string]bool)}
	for _, id := range ids {
		if id == identity.GenghisKhan {
			m.all = true
		}
		m.bases[id.IdentifierBase()] = true
		if p, ok := id.(identity.IsProbablyDependentProvider); ok {
			m.probably = append(m.probably, p)
		}
	}
	return m
}

func (m *changeMatcher) matches(deps []string, idxs []int) bool {
	if m.all {
		return true
	}
	for _, i := range idxs {
		if i < 0 || i >= len(deps) {
			continue
		}
		d := deps[i]
		if m.bases[d] {
			return true
		}
		for _, p := range m.probably {
			if p.IsProbablyDependent(identity.StringIdentity(d)) {
				return true
			}
		}
	}
	return false
}

func (h *HugoSites) incrementalFingerprint() (*buildState, error) {
	bs := &buildState{
		Version:     buildStateVersion,
		HugoVersion: hugo.CurrentVersion.String(),
		PublishDir:  h.incrementalPublishDir(),
		Files:       make(map[string]map[string]string),
	}

	hashContent := func(fi hugofs.FileMetaInfo) (string, error) {
		f, err := fi.Meta().Open()
		if err != nil {
			return "", err
		}
		defer f.Close()
		return hashing.XxHashFromReaderHexEncoded(f)
	}
	// Static files can be large and are copied verbatim; use size and mtime like rsync does.
	hashStat := func(fi hugofs.FileMetaInfo) (string, error) {
		return fmt.Sprintf("%d:%d", fi.Size(), fi.ModTime().UnixNano()), nil
	}

	type component struct {
		name       string
		fs         afero.Fs
		ignoreFile func(string) bool
		hash       func(hugofs.FileMetaInfo) (string, error)
	}

	sfs := h.BaseFs.SourceFilesystems
	components := []component{
		{files.ComponentFolderContent, sfs.Content.Fs, h.SourceSpec.IgnoreFile, hashContent},
		{files.ComponentFolderLayouts, sfs.Layouts.Fs, h.SourceSpec.IgnoreFile, hashContent},
		{files.ComponentFolderAssets, sfs.Assets.Fs, h.SourceSpec.IgnoreFile, hashContent},
		{files.ComponentFolderData, sfs.Data.Fs, h.SourceSpec.IgnoreFile, hashContent},
		{files.ComponentFolderI18n, sfs.I18n.Fs, h.SourceSpec.IgnoreFile, hashContent},
	}
	for lang, fs := range sfs.Static {
		components = append(components, component{staticComponent(lang), fs.Fs, nil, hashStat})
	}

	for _, c := range components {
		m := make(map[string]string)
		bs.Files[c.name] = m
		w := hugofs.NewWalkway(
			hugofs.WalkwayConfig{
				Fs:         c.fs,
				IgnoreFile: c.ignoreFile,
				PathParser: h.Conf.PathParser(),
				WalkFn: func(ctx context.Context, path string, fi hugofs.FileMetaInfo) error {
					if fi.IsDir() {
						return nil
					}
					hash, err := c.hash(fi)
					if err != nil {
						return err
					}
					m[paths.AddLeadingSlash(path)] = hash
					return nil
				},
			})
		if err := w.Walk(); err != nil {
			return nil, err
		}
	}

	confHashes := []string{
		h.Configs.Base.Environment,
		strings.Join(h.Configs.Base.RenderSegments, ","),
		fmt.Sprint(h.Configs.Base.CleanDestinationDir),
	}
	for _, f := range h.Configs.LoadingInfo.ConfigFiles {
		hashes, err := hashConfigFiles(f)
		if err != nil {
			return nil, err
		}
		confHashes = append(confHashes, hashes...)
	}
	bs.ConfigHash = hashing.XxHashFromStringHexEncoded(confHashes...)

	return bs, nil
}

// hashConfigFiles hashes the config file p, or, when p is a directory (e.g. config/_default),
// the config files below it.
func hashConfigFiles(p string) ([]string, error) {
	fi, err := hugofs.Os.Stat(p)
	if err != nil {
		return nil, err
	}
	if !fi.IsDir() {
		b, err := afero.ReadFile(hugofs.Os, p)
		if err != nil {
			return nil, err
		}
		return []string{hashing.XxHashFromStringHexEncoded(filepath.Base(p), string(b))}, nil
	}
	var hashes []string
	err = afero.Walk(hugofs.Os, p, func(path string, fi os.FileInfo, err error) error {
		if err != nil || fi.IsDir() || !config.IsValidConfigFilename(path) {
			return err
		}
		b, err := afero.ReadFile(hugofs.Os, path)
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(p, path)
		hashes = append(hashes, hashing.XxHashFromStringHexEncoded(filepath.ToSlash(rel), string(b)))
		return nil
	})
	return hashes, err
}

func (h *HugoSites) incrementalLoadState() (*buildState, error) {
	f, err := hugofs.Os.Open(h.incrementalStateFilename())
	if err != nil {
		return nil, nil
	}
	defer f.Close()
	r, err := gzip.NewReader(f)
	if err != nil {
		return nil, fmt.Errorf("failed to read build state: %w", err)
	}
	defer r.Close()
	var bs buildState
	if err := json.NewDecoder(r).Decode(&bs); err != nil {
		return nil, fmt.Errorf("failed to decode build state: %w", err)
	}
	if bs.Version != buildStateVersion {
		return nil, nil
	}
	return &bs, nil
}

// incrementalSaveState persists the build state after a successful build.
func (h *HugoSites) incrementalSaveState() error {
	ib := h.incremental
	if ib == nil || ib.cur == nil {
		return nil
	}
	bs := ib.cur

	depIdx := make(map[string]int)
	intern := func(s string) int {
		if i, ok := depIdx[s]; ok {
			return i
		}
		i := len(bs.Deps)
		bs.Deps = append(bs.Deps, s)
		depIdx[s] = i
		return i
	}

	h.withPage(func(key string, p *pageState) bool {
		sk := siteKey(p.s.resolveDimensionNames())
		for _, po := range p.pageOutputs {
			if !po.render {
				continue
			}
			if ib.skip != nil && ib.skip[incrementalSkipKey{p.s.siteVector, p.Path(), po.f.Name}] {
				// Not rendered this build; carry the previous state forward.
				if e, ok := ib.prevPages[statePageKey(sk, p.Path(), po.f.Name)]; ok {
					deps := make([]int, 0, len(e.Deps))
					for _, i := range e.Deps {
						if i >= 0 && i < len(ib.prev.Deps) {
							deps = append(deps, intern(ib.prev.Deps[i]))
						}
					}
					bs.Pages = append(bs.Pages, buildStatePage{Site: sk, Path: p.Path(), Format: po.f.Name, Target: e.Target, Deps: deps})
				}
				continue
			}
			if !po.isRendered() {
				continue
			}
			seen := make(map[string]struct{})
			var collect func(v any)
			collect = func(v any) {
				identity.WalkIdentitiesDeep(v, func(level int, id identity.Identity) bool {
					if ff, ok := id.(identity.FindFirstManagerIdentityProvider); ok {
						// E.g. a non-fingerprinted resource link. The one-of-many
						// optimization does not apply across builds, so track
						// the wrapped identity and manager.
						mi := ff.FindFirstManagerIdentity()
						if mi.Identity != nil {
							seen[mi.Identity.IdentifierBase()] = struct{}{}
						}
						if mi.Manager != nil {
							collect(mi.Manager)
						}
						return false
					}
					if b := id.IdentifierBase(); b != "" && b != identity.Anonymous.IdentifierBase() {
						seen[b] = struct{}{}
					}
					return false
				})
			}
			collect(po.dependencyManagerOutput)
			collect(p.dependencyManager)
			delete(seen, identity.Anonymous.IdentifierBase())
			bases := make([]string, 0, len(seen))
			for b := range seen {
				bases = append(bases, b)
			}
			sort.Strings(bases)
			deps := make([]int, len(bases))
			for i, b := range bases {
				deps[i] = intern(b)
			}
			bs.Pages = append(bs.Pages, buildStatePage{Site: sk, Path: p.Path(), Format: po.f.Name, Target: po.targetPaths().TargetFilename, Deps: deps})
		}
		return false
	})

	if h.Configs.Base.CleanDestinationDir && ib.prev != nil {
		targets := make(map[string]bool)
		for _, p := range bs.Pages {
			targets[p.Target] = true
		}
		var removed int
		for _, p := range ib.prev.Pages {
			if p.Target == "" || targets[p.Target] {
				continue
			}
			if err := removePublished(h.BaseFs.PublishFs, filepath.FromSlash(p.Target)); err != nil {
				h.Log.Warnf("incremental: failed to remove %q: %s", p.Target, err)
				continue
			}
			removed++
		}
		h.Log.Infof("incremental: removed %d stale page outputs", removed)
	}

	filename := h.incrementalStateFilename()
	if err := hugofs.Os.MkdirAll(filepath.Dir(filename), 0o777); err != nil {
		return err
	}
	f, err := hugofs.Os.Create(filename)
	if err != nil {
		return err
	}
	defer f.Close()
	w := gzip.NewWriter(f)
	if err := json.NewEncoder(w).Encode(bs); err != nil {
		return err
	}
	return w.Close()
}
