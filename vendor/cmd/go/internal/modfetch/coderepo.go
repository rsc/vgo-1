// Copyright 2018 The Go Authors. All rights reserved.
// Use of this source code is governed by a BSD-style
// license that can be found in the LICENSE file.

package modfetch

import (
	"archive/zip"
	"errors"
	"fmt"
	"io"
	"io/ioutil"
	"os"
	"path"
	"path/filepath"
	"regexp"
	"strings"
	"time"

	"cmd/go/internal/modfetch/codehost"
	"cmd/go/internal/modfile"
	"cmd/go/internal/module"
	"cmd/go/internal/semver"
)

// A codeRepo implements modfetch.Repo using an underlying codehost.Repo.
type codeRepo struct {
	modPath string

	code     codehost.Repo
	codeRoot string
	codeDir  string

	path        string
	pathPrefix  string
	pathMajor   string
	pseudoMajor string
}

func newCodeRepo(code codehost.Repo, root, path string) (Repo, error) {
	if !hasPathPrefix(path, root) {
		return nil, fmt.Errorf("mismatched repo: found %s for %s", root, path)
	}
	pathPrefix, pathMajor, ok := module.SplitPathVersion(path)
	if !ok {
		return nil, fmt.Errorf("invalid module path %q", path)
	}
	pseudoMajor := "v0"
	if pathMajor != "" {
		pseudoMajor = pathMajor[1:]
	}

	// At this point we might have:
	//	codeRoot = github.com/rsc/foo
	//	path = github.com/rsc/foo/bar/v2
	//	pathPrefix = github.com/rsc/foo/bar
	//	pathMajor = /v2
	//	pseudoMajor = v2
	//
	// Compute codeDir = bar, the subdirectory within the repo
	// corresponding to the module root.
	codeDir := strings.Trim(strings.TrimPrefix(pathPrefix, root), "/")
	if strings.HasPrefix(path, "gopkg.in/") {
		// But gopkg.in is a special legacy case, in which pathPrefix does not start with codeRoot.
		// For example we might have:
		//	codeRoot = gopkg.in/yaml.v2
		//	pathPrefix = gopkg.in/yaml
		//	pathMajor = .v2
		//	pseudoMajor = v2
		//	codeDir = pathPrefix (because codeRoot is not a prefix of pathPrefix)
		// Clear codeDir - the module root is the repo root for gopkg.in repos.
		codeDir = ""
	}

	r := &codeRepo{
		modPath:     path,
		code:        code,
		codeRoot:    root,
		codeDir:     codeDir,
		pathPrefix:  pathPrefix,
		pathMajor:   pathMajor,
		pseudoMajor: pseudoMajor,
	}

	return r, nil
}

func (r *codeRepo) ModulePath() string {
	return r.modPath
}

func (r *codeRepo) Versions(prefix string) ([]string, error) {
	p := prefix
	if r.codeDir != "" {
		p = r.codeDir + "/" + p
	}
	tags, err := r.code.Tags(p)
	if err != nil {
		return nil, err
	}
	list := []string{}
	for _, tag := range tags {
		if !strings.HasPrefix(tag, p) {
			continue
		}
		v := tag
		if r.codeDir != "" {
			v = v[len(r.codeDir)+1:]
		}
		if !semver.IsValid(v) || v != semver.Canonical(v) || IsPseudoVersion(v) || !module.MatchPathMajor(v, r.pathMajor) {
			continue
		}
		list = append(list, v)
	}
	SortVersions(list)
	return list, nil
}

func (r *codeRepo) Stat(rev string) (*RevInfo, error) {
	if rev == "latest" {
		return r.Latest()
	}
	codeRev := r.revToRev(rev)
	if semver.IsValid(codeRev) && r.codeDir != "" {
		codeRev = r.codeDir + "/" + codeRev
	}
	info, err := r.code.Stat(codeRev)
	if err != nil {
		return nil, err
	}
	return r.convert(info)
}

func (r *codeRepo) Latest() (*RevInfo, error) {
	info, err := r.code.Latest()
	if err != nil {
		return nil, err
	}
	return r.convert(info)
}

func (r *codeRepo) convert(info *codehost.RevInfo) (*RevInfo, error) {
	versionOK := func(v string) bool {
		return semver.IsValid(v) && v == semver.Canonical(v) && !IsPseudoVersion(v) && module.MatchPathMajor(v, r.pathMajor)
	}
	v := info.Version
	if r.codeDir == "" {
		if !versionOK(v) {
			v = PseudoVersion(r.pseudoMajor, info.Time, info.Short)
		}
	} else {
		p := r.codeDir + "/"
		if strings.HasPrefix(v, p) && versionOK(v[len(p):]) {
			v = v[len(p):]
		} else {
			v = PseudoVersion(r.pseudoMajor, info.Time, info.Short)
		}
	}

	info2 := &RevInfo{
		Name:    info.Name,
		Short:   info.Short,
		Time:    info.Time,
		Version: v,
	}
	return info2, nil
}

func (r *codeRepo) revToRev(rev string) string {
	if semver.IsValid(rev) {
		if IsPseudoVersion(rev) {
			i := strings.Index(rev, "-")
			j := strings.Index(rev[i+1:], "-")
			return rev[i+1+j+1:]
		}
		if r.codeDir == "" {
			return rev
		}
		return r.codeDir + "/" + rev
	}
	return rev
}

func (r *codeRepo) versionToRev(version string) (rev string, err error) {
	if !semver.IsValid(version) {
		return "", fmt.Errorf("malformed semantic version %q", version)
	}
	return r.revToRev(version), nil
}

func (r *codeRepo) findDir(version string) (rev, dir string, gomod []byte, err error) {
	rev, err = r.versionToRev(version)
	if err != nil {
		return "", "", nil, err
	}
	if r.pathMajor == "" || strings.HasPrefix(r.pathMajor, ".") {
		if r.codeDir == "" {
			return rev, "", nil, nil
		}
		file1 := path.Join(r.codeDir, "go.mod")
		gomod1, err1 := r.code.ReadFile(rev, file1, codehost.MaxGoMod)
		if err1 != nil {
			if os.IsNotExist(err1) {
				return "", "", nil, errors.New("missing go.mod")
			}
			return "", "", nil, fmt.Errorf("reading go.mod: %v", err1)
		}
		return rev, r.codeDir, gomod1, nil
	}

	// Suppose pathMajor is "/v2".
	// Either go.mod should claim v2 and v2/go.mod should not exist,
	// or v2/go.mod should exist and claim v2. Not both.
	// Note that we don't check the full path, just the major suffix,
	// because of replacement modules. This might be a fork of
	// the real module, found at a different path, usable only in
	// a replace directive.
	file1 := path.Join(r.codeDir, "go.mod")
	file2 := path.Join(r.codeDir, r.pathMajor[1:], "go.mod")
	gomod1, err1 := r.code.ReadFile(rev, file1, codehost.MaxGoMod)
	gomod2, err2 := r.code.ReadFile(rev, file2, codehost.MaxGoMod)

	if err1 != nil && !os.IsNotExist(err1) {
		return "", "", nil, fmt.Errorf("reading %s: %v", file1, err1)
	}
	if err2 != nil && !os.IsNotExist(err2) {
		return "", "", nil, fmt.Errorf("reading %s: %v", file2, err2)
	}

	found1 := err1 == nil && isMajor(gomod1, r.pathMajor)
	found2 := err2 == nil && isMajor(gomod2, r.pathMajor)

	if err2 == nil && !found2 {
		return "", "", nil, fmt.Errorf("%s has non-...%s module path", file2, r.pathMajor)
	}
	if found1 && found2 {
		return "", "", nil, fmt.Errorf("both %s and %s claim ...%s module", file1, file2, r.pathMajor)
	}
	if found2 {
		return rev, filepath.Join(r.codeDir, r.pathMajor), gomod2, nil
	}
	if found1 {
		return rev, r.codeDir, gomod1, nil
	}
	return "", "", nil, fmt.Errorf("missing or invalid go.mod")
}

func isMajor(gomod []byte, pathMajor string) bool {
	return strings.HasSuffix(modfile.ModulePath(gomod), pathMajor)
}

func (r *codeRepo) GoMod(version string) (data []byte, err error) {
	rev, dir, gomod, err := r.findDir(version)
	if err != nil {
		return nil, err
	}
	if gomod != nil {
		return gomod, nil
	}
	data, err = r.code.ReadFile(rev, path.Join(dir, "go.mod"), codehost.MaxGoMod)
	if err != nil {
		if os.IsNotExist(err) {
			return r.legacyGoMod(rev, dir), nil
		}
		return nil, err
	}
	return data, nil
}

func (r *codeRepo) legacyGoMod(rev, dir string) []byte {
	// We used to try to build a go.mod reflecting pre-existing
	// package management metadata files, but the conversion
	// was inherently imperfect (because those files don't have
	// exactly the same semantics as go.mod) and, when done
	// for dependencies in the middle of a build, impossible to
	// correct. So we stopped.
	// Return a fake go.mod that simply declares the module path.
	return []byte(fmt.Sprintf("module %s\n", modfile.AutoQuote(r.modPath)))
}

func (r *codeRepo) modPrefix(rev string) string {
	return r.modPath + "@" + rev
}

func (r *codeRepo) Zip(version string, tmpdir string) (tmpfile string, err error) {
	rev, dir, _, err := r.findDir(version)
	if err != nil {
		return "", err
	}
	dl, actualDir, err := r.code.ReadZip(rev, dir, codehost.MaxZipFile)
	if err != nil {
		return "", err
	}
	if actualDir != "" && !hasPathPrefix(dir, actualDir) {
		return "", fmt.Errorf("internal error: downloading %v %v: dir=%q but actualDir=%q", r.path, rev, dir, actualDir)
	}
	subdir := strings.Trim(strings.TrimPrefix(dir, actualDir), "/")

	// Spool to local file.
	f, err := ioutil.TempFile(tmpdir, "vgo-codehost-")
	if err != nil {
		dl.Close()
		return "", err
	}
	defer os.Remove(f.Name())
	defer f.Close()
	maxSize := int64(codehost.MaxZipFile)
	lr := &io.LimitedReader{R: dl, N: maxSize + 1}
	if _, err := io.Copy(f, lr); err != nil {
		dl.Close()
		return "", err
	}
	dl.Close()
	if lr.N <= 0 {
		return "", fmt.Errorf("downloaded zip file too large")
	}
	size := (maxSize + 1) - lr.N
	if _, err := f.Seek(0, 0); err != nil {
		return "", err
	}

	// Translate from zip file we have to zip file we want.
	zr, err := zip.NewReader(f, size)
	if err != nil {
		return "", err
	}
	f2, err := ioutil.TempFile(tmpdir, "vgo-")
	if err != nil {
		return "", err
	}

	zw := zip.NewWriter(f2)
	newName := f2.Name()
	defer func() {
		f2.Close()
		if err != nil {
			os.Remove(newName)
		}
	}()
	if subdir != "" {
		subdir += "/"
	}
	haveLICENSE := false
	topPrefix := ""
	haveGoMod := make(map[string]bool)
	for _, zf := range zr.File {
		if topPrefix == "" {
			i := strings.Index(zf.Name, "/")
			if i < 0 {
				return "", fmt.Errorf("missing top-level directory prefix")
			}
			topPrefix = zf.Name[:i+1]
		}
		if !strings.HasPrefix(zf.Name, topPrefix) {
			return "", fmt.Errorf("zip file contains more than one top-level directory")
		}
		dir, file := path.Split(zf.Name)
		if file == "go.mod" {
			haveGoMod[dir] = true
		}
	}
	root := topPrefix + subdir
	inSubmodule := func(name string) bool {
		for {
			dir, _ := path.Split(name)
			if len(dir) <= len(root) {
				return false
			}
			if haveGoMod[dir] {
				return true
			}
			name = dir[:len(dir)-1]
		}
	}
	for _, zf := range zr.File {
		if topPrefix == "" {
			i := strings.Index(zf.Name, "/")
			if i < 0 {
				return "", fmt.Errorf("missing top-level directory prefix")
			}
			topPrefix = zf.Name[:i+1]
		}
		if strings.HasSuffix(zf.Name, "/") { // drop directory dummy entries
			continue
		}
		if !strings.HasPrefix(zf.Name, topPrefix) {
			return "", fmt.Errorf("zip file contains more than one top-level directory")
		}
		name := strings.TrimPrefix(zf.Name, topPrefix)
		if !strings.HasPrefix(name, subdir) {
			continue
		}
		if name == ".hg_archival.txt" {
			// Inserted by hg archive.
			// Not correct to drop from other version control systems, but too bad.
			continue
		}
		name = strings.TrimPrefix(name, subdir)
		if isVendoredPackage(name) {
			continue
		}
		if inSubmodule(zf.Name) {
			continue
		}
		base := path.Base(name)
		if strings.ToLower(base) == "go.mod" && base != "go.mod" {
			return "", fmt.Errorf("zip file contains %s, want all lower-case go.mod", zf.Name)
		}
		if name == "LICENSE" {
			haveLICENSE = true
		}
		size := int64(zf.UncompressedSize)
		if size < 0 || maxSize < size {
			return "", fmt.Errorf("module source tree too big")
		}
		maxSize -= size

		rc, err := zf.Open()
		if err != nil {
			return "", err
		}
		w, err := zw.Create(r.modPrefix(version) + "/" + name)
		lr := &io.LimitedReader{R: rc, N: size + 1}
		if _, err := io.Copy(w, lr); err != nil {
			return "", err
		}
		if lr.N <= 0 {
			return "", fmt.Errorf("individual file too large")
		}
	}

	if !haveLICENSE && subdir != "" {
		data, err := r.code.ReadFile(rev, "LICENSE", codehost.MaxLICENSE)
		if err == nil {
			w, err := zw.Create(r.modPrefix(version) + "/LICENSE")
			if err != nil {
				return "", err
			}
			if _, err := w.Write(data); err != nil {
				return "", err
			}
		}
	}
	if err := zw.Close(); err != nil {
		return "", err
	}
	if err := f2.Close(); err != nil {
		return "", err
	}

	return f2.Name(), nil
}

// hasPathPrefix reports whether the path s begins with the
// elements in prefix.
func hasPathPrefix(s, prefix string) bool {
	switch {
	default:
		return false
	case len(s) == len(prefix):
		return s == prefix
	case len(s) > len(prefix):
		if prefix != "" && prefix[len(prefix)-1] == '/' {
			return strings.HasPrefix(s, prefix)
		}
		return s[len(prefix)] == '/' && s[:len(prefix)] == prefix
	}
}

func isVendoredPackage(name string) bool {
	var i int
	if strings.HasPrefix(name, "vendor/") {
		i += len("vendor/")
	} else if j := strings.Index(name, "/vendor/"); j >= 0 {
		i += len("/vendor/")
	} else {
		return false
	}
	return strings.Contains(name[i:], "/")
}

func PseudoVersion(major string, t time.Time, rev string) string {
	if major == "" {
		major = "v0"
	}
	return fmt.Sprintf("%s.0.0-%s-%s", major, t.UTC().Format("20060102150405"), rev)
}

var ErrNotPseudoVersion = errors.New("not a pseudo-version")

/*
func ParsePseudoVersion(repo Repo, version string) (rev string, err error) {
	major := semver.Major(version)
	if major == "" {
		return "", ErrNotPseudoVersion
	}
	majorPrefix := major + ".0.0-"
	if !strings.HasPrefix(version, majorPrefix) || !strings.Contains(version[len(majorPrefix):], "-") {
		return "", ErrNotPseudoVersion
	}
	versionSuffix := version[len(majorPrefix):]
	for i := 0; versionSuffix[i] != '-'; i++ {
		c := versionSuffix[i]
		if c < '0' || '9' < c {
			return "", ErrNotPseudoVersion
		}
	}
	rev = versionSuffix[strings.Index(versionSuffix, "-")+1:]
	if rev == "" {
		return "", ErrNotPseudoVersion
	}
	if proxyURL != "" {
		return version, nil
	}
	fullRev, t, err := repo.CommitInfo(rev)
	if err != nil {
		return "", fmt.Errorf("unknown pseudo-version %s: loading %v: %v", version, rev, err)
	}
	v := PseudoVersion(major, t, repo.ShortRev(fullRev))
	if v != version {
		return "", fmt.Errorf("unknown pseudo-version %s: %v is %v", version, rev, v)
	}
	return fullRev, nil
}
*/

var pseudoVersionRE = regexp.MustCompile(`^v[0-9]+\.0\.0-[0-9]{14}-[A-Za-z0-9]+$`)

// IsPseudoVersion reports whether v is a pseudo-version.
func IsPseudoVersion(v string) bool {
	return pseudoVersionRE.MatchString(v)
}

// PseudoVersionTime returns the time stamp of the pseudo-version v.
// It returns an error if v is not a pseudo-version or if the time stamp
// embedded in the pseudo-version is not a valid time.
func PseudoVersionTime(v string) (time.Time, error) {
	if !IsPseudoVersion(v) {
		return time.Time{}, fmt.Errorf("not a pseudo-version")
	}
	i := strings.Index(v, "-") + 1
	j := i + strings.Index(v[i:], "-")
	t, err := time.Parse("20060102150405", v[i:j])
	if err != nil {
		return time.Time{}, fmt.Errorf("malformed pseudo-version %q", v)
	}
	return t, nil
}
