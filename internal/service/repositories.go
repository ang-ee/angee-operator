package service

import (
	"bytes"
	"encoding/json"
	"fmt"
	"maps"
	"path"
	"regexp"
	"sort"
	"strings"
	"unicode"

	"github.com/ang-ee/angee-operator/internal/manifest"
)

// The stack template inputs that list the repositories a stack is made of.
// A stack template that declares one loops over its entries to render the
// stack's sources. RepositoriesInput lists the repositories whose addons the
// stack runs, ExtraSourcesInput the ones it only checks out; the operator
// reads both the same way and leaves the difference to the template.
const (
	RepositoriesInput = "repositories"
	ExtraSourcesInput = "extra_sources"
)

// repositoryInputFlags names, for each repository list input, the CLI flag
// that fills it, as a refusal names it.
var repositoryInputFlags = map[string]string{
	RepositoriesInput: "--repository",
	ExtraSourcesInput: "--extra-source",
}

// repositoryInputNames lists the repository list inputs in the order they are
// read, which is the order a refusal that concerns two of them names them in.
var repositoryInputNames = []string{RepositoriesInput, ExtraSourcesInput}

// repositoryEntry is one repository of a repository list input in its
// canonical form, the one a stack template loops over. Its JSON keys are
// encoded in this order, which is also their alphabetical order, so an entry
// decoded into a map and encoded again reads the same.
type repositoryEntry struct {
	// Name is the repository's name: the last element of its path, without
	// `.git`. A template uses it as the source's name, and with it the slot's,
	// the cache directory's and the checkout's.
	Name string `json:"name"`
	// Ref is the ref the repository was given with (`<repo>@<ref>`), or "".
	Ref string `json:"ref"`
	// Repo is the URL the repository is cloned from, as it is written to
	// `sources.<name>.repo`.
	Repo string `json:"repo"`
	// SSH says the URL is reached over ssh: the scp-like form, or an ssh URL.
	// A template gives such a source a key (`auth.ssh_key_secret`).
	SSH bool `json:"ssh"`
}

var (
	repositorySchemeRE = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9+.-]*://`)
	repositoryNameRE   = regexp.MustCompile(`^[a-z0-9][a-z0-9-]*$`)
)

// parseRepository reads one repository as `<repo>[@<ref>]`. <repo> is, in the
// order tried:
//
//  1. a URL with a scheme (`https://host/owner/name.git`, `ssh://…`,
//     `file:///…`), whose path is what follows the host; the scheme is
//     recorded in lowercase;
//  2. the scp-like form (`[user@]host:owner/name.git`, a `:` before the first
//     `/`, as manifest.IsRelativeURL tells it), whose path is what follows
//     that `:`;
//  3. an absolute path;
//  4. `owner/name`, a repository on GitHub: `https://github.com/owner/name.git`.
//
// Anything else is refused. The ref is what follows the first `@` of the
// path, so it may hold a `/` as branch names do (`owner/name@feature/x`); a
// template's own `name@pin` splits at the last `@` and takes no `/`, since a
// template name holds one. An `@` before the path, as in `git@host:…`, is the
// URL's. A path that itself holds an `@` cannot be given.
//
// A URL is written to angee.yaml and to the answers file, so one that carries
// credentials is refused: a password under any scheme, any user under a
// scheme other than ssh, where a user name is often a token, and, in any
// form, a `?` or a `#` and what follows it. No refusal quotes them.
func parseRepository(spec string) (repositoryEntry, error) {
	var entry repositoryEntry
	if spec == "" {
		return entry, fmt.Errorf("a repository is empty")
	}
	// shown is the repository as a refusal quotes it: without what may be a
	// credential, a URL's user and what follows a `?` or a `#`.
	shown := spec
	if cut := strings.IndexAny(shown, "?#"); cut >= 0 {
		shown = shown[:cut] + "…"
	}
	var prefix, repoPath string
	switch {
	case repositorySchemeRE.MatchString(spec):
		scheme, rest, _ := strings.Cut(spec, "://")
		scheme = strings.ToLower(scheme)
		authority, pathPart, _ := strings.Cut(rest, "/")
		authority, _, _ = strings.Cut(authority, "?")
		authority, _, _ = strings.Cut(authority, "#")
		user, host := "", authority
		if at := strings.LastIndex(authority, "@"); at >= 0 {
			user, host = authority[:at], authority[at+1:]
		}
		masked := scheme + "://" + host + "/" + pathPart
		if user != "" {
			masked = scheme + "://***@" + host + "/" + pathPart
		}
		if cut := strings.IndexAny(masked, "?#"); cut >= 0 {
			masked = masked[:cut] + "…"
		}
		shown = masked
		switch {
		case strings.Contains(user, ":"):
			return entry, fmt.Errorf("repository %q carries a password in its URL, which would be written to angee.yaml and to the answers file; give the URL without it, and the credential through the source's auth (sources.<name>.auth)", shown)
		case user != "" && !manifest.IsSSHScheme(scheme):
			return entry, fmt.Errorf("repository %q carries a user in its URL, which over %s is often a token, and would be written to angee.yaml and to the answers file; give the URL without it", shown, scheme)
		}
		// An ssh URL's user, which is no credential, is quoted as given.
		if user != "" {
			shown = strings.Replace(shown, "://***@", "://"+user+"@", 1)
		}
		if pathPart == "" {
			return entry, fmt.Errorf("repository %q has no path after its host", shown)
		}
		prefix, repoPath = scheme+"://"+authority, "/"+pathPart
		entry.SSH = manifest.IsSSHScheme(scheme)
	case !strings.HasPrefix(spec, "/") && !strings.Contains(spec, "://") && !manifest.IsRelativeURL(spec):
		host, rest, _ := strings.Cut(spec, ":")
		prefix, repoPath = host+":", rest
		entry.SSH = true
	default:
		repoPath = spec
	}
	// A URL's query or fragment is where a token is sometimes passed, and
	// git reads neither in the other forms.
	if strings.ContainsAny(spec, "?#") {
		return entry, fmt.Errorf("repository %q holds a '?' or a '#': what follows one is no part of a repository, would be written to angee.yaml and to the answers file, and in a URL is often a token; give it without", shown)
	}
	if strings.ContainsFunc(spec, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }) {
		return entry, fmt.Errorf("repository %q contains a space or a control character", shown)
	}
	// git would read it as an option, or refuse it as a strange host name.
	if strings.HasPrefix(spec, "-") {
		return entry, fmt.Errorf("repository %q starts with '-'", shown)
	}
	repoPath, ref, pinned := strings.Cut(repoPath, "@")
	if pinned {
		if err := checkRepositoryRef(ref); err != nil {
			return entry, fmt.Errorf("repository %q: ref %q %w", shown, ref, err)
		}
		entry.Ref = ref
	}
	if prefix == "" && !strings.HasPrefix(repoPath, "/") {
		owner, name, ok := strings.Cut(repoPath, "/")
		if !ok || !gitHubOwnerRE.MatchString(owner) || !gitHubRepoRE.MatchString(name) || name == "." || name == ".." {
			return entry, fmt.Errorf("%q is not a repository: give a URL (https://host/owner/name.git), an ssh remote (git@host:owner/name.git), an absolute path, or a GitHub owner/name, each optionally followed by @<ref>", shown)
		}
		prefix, repoPath = "https://github.com/", owner+"/"+strings.TrimSuffix(name, ".git")+".git"
	}
	entry.Repo = prefix + repoPath
	entry.Name = strings.TrimSuffix(path.Base(strings.TrimRight(repoPath, "/")), ".git")
	if !repositoryNameRE.MatchString(entry.Name) {
		return entry, fmt.Errorf("repository %q: its name %q, the last element of its path without .git, must start with a lowercase letter or a digit and hold only lowercase letters, digits and '-', since it names the stack's source, its slot and its directories", shown, entry.Name)
	}
	// The source owns what a remote URL may be.
	if err := manifest.ValidateSource(entry.Name, manifest.Source{Kind: "git", Repo: entry.Repo}); err != nil {
		return entry, fmt.Errorf("repository %q: %w", shown, err)
	}
	return entry, nil
}

// checkRepositoryRef refuses a ref git would not take as a branch or tag
// name, or would read as an option or as two refs. Its message follows the
// ref's name.
func checkRepositoryRef(ref string) error {
	switch {
	case ref == "":
		return fmt.Errorf("is empty: leave out the @ to take the default branch")
	case strings.ContainsFunc(ref, func(r rune) bool { return unicode.IsSpace(r) || unicode.IsControl(r) }):
		return fmt.Errorf("holds a space or a control character")
	case strings.HasPrefix(ref, "-") || strings.HasPrefix(ref, "/"):
		return fmt.Errorf("starts with '-' or '/'")
	case strings.HasSuffix(ref, "/") || strings.HasSuffix(ref, "."):
		return fmt.Errorf("ends with '/' or '.'")
	case strings.Contains(ref, "..") || strings.Contains(ref, "//") || strings.Contains(ref, "@{") || ref == "@":
		return fmt.Errorf("holds '..', '//' or '@{', or is '@'")
	case strings.ContainsAny(ref, "~^:?*[\\"):
		return fmt.Errorf("holds one of ~ ^ : ? * [ \\")
	}
	for _, element := range strings.Split(ref, "/") {
		if strings.HasPrefix(element, ".") || strings.HasSuffix(element, ".lock") {
			return fmt.Errorf("has an element that starts with '.' or ends with '.lock'")
		}
	}
	return nil
}

// parseRepositoryList reads a repository list input: a JSON array whose
// entries are each a `<repo>[@<ref>]` string or an object `{repo, ref}`. An
// object may also carry the `name` and `ssh` the canonical form adds, as an
// answer recorded by an earlier render does; they must be the ones its repo
// gives. An empty value is the empty list. The entries are returned sorted by
// name, and a name listed twice is refused.
func parseRepositoryList(input, raw string) ([]repositoryEntry, error) {
	invalid := func(format string, args ...any) error {
		return &InvalidInputError{Field: input, Reason: fmt.Sprintf(format, args...)}
	}
	entries := []repositoryEntry{}
	if strings.TrimSpace(raw) == "" {
		return entries, nil
	}
	var items []json.RawMessage
	if err := json.Unmarshal([]byte(raw), &items); err != nil {
		return nil, invalid("must be a JSON array of repositories, each a \"<repo>[@<ref>]\" string or an object {\"repo\": …, \"ref\": …}: %v", err)
	}
	seen := map[string]bool{}
	for index, item := range items {
		entry, err := parseRepositoryItem(item)
		if err != nil {
			return nil, invalid("entry %d: %v", index+1, err)
		}
		if seen[entry.Name] {
			return nil, invalid("lists repository %q twice: a stack has one source of a name", entry.Name)
		}
		seen[entry.Name] = true
		entries = append(entries, entry)
	}
	sort.Slice(entries, func(i, j int) bool { return entries[i].Name < entries[j].Name })
	return entries, nil
}

// parseRepositoryItem reads one entry of a repository list (parseRepositoryList).
func parseRepositoryItem(item json.RawMessage) (repositoryEntry, error) {
	var spec string
	if err := json.Unmarshal(item, &spec); err == nil {
		return parseRepository(spec)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(item, &object); err != nil || object == nil {
		return repositoryEntry{}, fmt.Errorf("must be a \"<repo>[@<ref>]\" string or an object {\"repo\": …, \"ref\": …}")
	}
	text := func(key string) (string, error) {
		raw, ok := object[key]
		if !ok || string(raw) == "null" {
			return "", nil
		}
		var value string
		if err := json.Unmarshal(raw, &value); err != nil {
			return "", fmt.Errorf("%s must be a string", key)
		}
		return value, nil
	}
	for _, key := range sortedKeys(object) {
		switch key {
		case "name", "ref", "repo", "ssh":
		default:
			return repositoryEntry{}, fmt.Errorf("has an unknown key %q: an entry takes repo and ref", key)
		}
	}
	repo, err := text("repo")
	if err != nil {
		return repositoryEntry{}, err
	}
	if repo == "" {
		return repositoryEntry{}, fmt.Errorf("an object needs a repo")
	}
	entry, err := parseRepository(repo)
	if err != nil {
		return repositoryEntry{}, err
	}
	ref, err := text("ref")
	if err != nil {
		return repositoryEntry{}, err
	}
	switch {
	case ref == "":
	case entry.Ref != "" && entry.Ref != ref:
		return repositoryEntry{}, fmt.Errorf("repository %q names ref %q in its repo and %q as its ref", entry.Name, entry.Ref, ref)
	default:
		if err := checkRepositoryRef(ref); err != nil {
			return repositoryEntry{}, fmt.Errorf("repository %q: ref %q %w", entry.Name, ref, err)
		}
		entry.Ref = ref
	}
	name, err := text("name")
	if err != nil {
		return repositoryEntry{}, err
	}
	if name != "" && name != entry.Name {
		return repositoryEntry{}, fmt.Errorf("the repository is named %q by its path, not %q: a repository's name is not chosen", entry.Name, name)
	}
	if raw, ok := object["ssh"]; ok && string(raw) != "null" {
		var given bool
		if err := json.Unmarshal(raw, &given); err != nil {
			return repositoryEntry{}, fmt.Errorf("ssh must be true or false")
		}
		if given != entry.SSH {
			return repositoryEntry{}, fmt.Errorf("repository %q: ssh is %t by its URL, not %t", entry.Name, entry.SSH, given)
		}
	}
	return entry, nil
}

// encodeRepositoryList is the canonical value of a repository list input: a
// JSON array of the entries, sorted by name, each with the keys name, ref,
// repo and ssh in that order. parseRepositoryList reads it back to the same
// entries, so encoding a recorded value again yields the same bytes.
func encodeRepositoryList(entries []repositoryEntry) string {
	if len(entries) == 0 {
		return "[]"
	}
	var out bytes.Buffer
	encoder := json.NewEncoder(&out)
	// A URL is recorded as written, not with & and the like escaped.
	encoder.SetEscapeHTML(false)
	// The entries hold only strings and a boolean, which always encode.
	_ = encoder.Encode(entries)
	return strings.TrimSuffix(out.String(), "\n")
}

// canonicalizeRepositoryInputs rewrites the repository list inputs among
// values in their canonical form (encodeRepositoryList), in place, so a
// template loops over the same entries whether a list came from
// `--repository`, `--input`, an answers file, the API or the answers an
// earlier render recorded. A repository named in both lists is refused: its
// name would be one source twice.
//
// A list given to a template that declares no such input is refused, rather
// than rendered into nothing; an empty one, which asks for nothing, is left
// as it is. With recorded, values are the answers an earlier render
// recorded, and a list the template no longer declares is left as it is, as
// any other answer it no longer asks for.
func canonicalizeRepositoryInputs(templateRef, templatePath string, values map[string]string, recorded bool) error {
	var declared map[string]bool
	listed := map[string]string{}
	for _, input := range repositoryInputNames {
		raw, given := values[input]
		if !given {
			continue
		}
		if declared == nil {
			descriptor, err := templateDescriptor(templateRef, templatePath)
			if err != nil {
				return err
			}
			declared = map[string]bool{}
			for _, def := range descriptor.Inputs {
				declared[def.Name] = true
			}
		}
		if !declared[input] {
			if recorded || strings.TrimSpace(raw) == "" || strings.TrimSpace(raw) == "[]" {
				continue
			}
			return &InvalidInputError{Field: input, Reason: fmt.Sprintf("stack template %q declares no input %q, so it takes no %s", templateRef, input, repositoryInputFlags[input])}
		}
		entries, err := parseRepositoryList(input, raw)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if other, dup := listed[entry.Name]; dup {
				return &InvalidInputError{Field: input, Reason: fmt.Sprintf("repository %q is in both %s and %s: a stack has one source of a name", entry.Name, other, input)}
			}
			listed[entry.Name] = input
		}
		values[input] = encodeRepositoryList(entries)
	}
	return nil
}

// recordedRepositoryLists returns the answers an earlier render recorded with
// their repository lists in canonical form, as they are replayed and shown.
// copier records a list as the YAML of its entries, which reads back as
// another spelling of the same value.
//
// A recorded list that no longer reads as one (it was edited, or the rules
// have changed since) is an error only when it is replayed: one that a list
// in given replaces is returned as recorded, so it cannot stand in the way
// of its own replacement.
func recordedRepositoryLists(templateRef, templatePath string, answers, given map[string]string) (map[string]string, error) {
	canonical := maps.Clone(answers)
	if canonicalizeRepositoryInputs(templateRef, templatePath, canonical, true) == nil {
		return canonical, nil
	}
	replayed := map[string]string{}
	for _, input := range repositoryInputNames {
		if value, recorded := answers[input]; recorded {
			if _, replaced := given[input]; !replaced {
				replayed[input] = value
			}
		}
	}
	if err := canonicalizeRepositoryInputs(templateRef, templatePath, replayed, true); err != nil {
		return nil, err
	}
	canonical = maps.Clone(answers)
	maps.Copy(canonical, replayed)
	return canonical, nil
}
