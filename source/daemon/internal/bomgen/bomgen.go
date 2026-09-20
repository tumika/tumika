// Package bomgen builds the bills of materials a release host serves.
//
// Its input is the list of releases the GitHub Releases API reports, with the
// digest of every asset; its output is the exact bytes published at
// /releases/<label>.json and /channels/<channel>.json, ready to be signed. The
// documents are built out of the types in platform/release and every one is
// parsed back through release.ParseBOM before it is returned, so the producer
// cannot publish a document its reader would refuse.
//
// Generation is pure: same input, same bytes. A failed publish is retried by
// running the generator again over the same releases, which is only safe
// because nothing here depends on when it ran.
package bomgen

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"sort"
	"strings"
	"time"

	"github.com/tumika/tumika/source/daemon/internal/platform/release"
)

// Release is one release as the Releases API reports it, carrying the digest of
// every asset and the content of the release.yaml the release was built from.
type Release struct {
	// Tag is the git tag the release was cut from: v<label> for a calendar
	// release, edge-<run number> for an edge build.
	Tag string
	// Draft is true while the release is still being assembled. A draft is not
	// downloadable, so nothing may point at its assets, and it is left out of
	// the result without being reported as a Skip: it is not published yet,
	// which is not a failure.
	Draft bool
	// Prerelease is the flag the release carries on GitHub. The release
	// workflow sets it from the label, so it must agree with the tag.
	Prerelease bool
	// PublishedAt orders releases. Recency, not the label, decides which
	// release a channel offers.
	PublishedAt time.Time
	// Assets are the files the release publishes.
	Assets []Asset
	// ReleaseYAML is the content of the release.yaml asset: the label and the
	// component versions this release was built from. Empty when the release
	// carries none, which makes the release unpublishable.
	ReleaseYAML []byte
}

// Asset is one file a release publishes, with the digest of the bytes served.
type Asset struct {
	Name   string
	URL    string
	SHA256 string
}

// Document is one file to publish, and the bytes to sign.
type Document struct {
	// Path is where the document belongs, relative to the site root.
	Path string
	// Release is the label the document describes.
	Release string
	// Channel is the channel the document declares. For a channel head it is
	// the channel the document is served under, which is not necessarily the
	// channel the release was cut into.
	Channel release.Channel
	// Bytes are what gets published and what gets signed, byte for byte.
	Bytes []byte
}

// Skip records a release that was not published and why. A draft is not a
// skip: it is ignored, so an unfinished release never fails a publish run.
//
// A release is skipped whole: an incomplete one is never published half-built,
// because a BOM missing an asset is a daemon that cannot update.
type Skip struct {
	Tag    string
	Reason string
}

// Result is everything a publish run writes, plus what it left out.
type Result struct {
	// Releases is one document per published release, oldest first.
	Releases []Document
	// Channels is one head document per channel that has a release, in
	// release.Channels order. A channel offering nothing has no document: a
	// 404 is what the daemon reads as "nothing published", and an empty
	// document is not a shape it accepts.
	Channels []Document
	// Skipped is every release that was not published, in the order they were
	// given.
	Skipped []Skip
}

// componentBinaries names the file each component publishes its raw binary
// under: <binary>_<component version>_<goos>_<goarch>, the name template in
// source/daemon/.goreleaser.yml.
//
// A release naming a component absent from this map is skipped rather than
// published without it — a component whose asset name nobody knows is a
// component no client can fetch.
var componentBinaries = map[string]string{
	release.DaemonComponent: "tumika",
}

// A platform suffix is Go's own GOOS and GOARCH, which carry no separator and
// no dot — the dot is what tells the raw binary apart from the .tar.gz archive
// built for the same platform.
var platformPartPattern = regexp.MustCompile(`^[a-z0-9]+$`)

// edgeTagPrefix is what an edge tag is spelled with. It never matches the
// release workflow's v*.*.* glob, so an edge build is cut without a calendar
// tag existing for it.
const edgeTagPrefix = "edge-"

// ReleasePath is where a release's own bill of materials is served.
func ReleasePath(label string) string { return "releases/" + label + ".json" }

// ChannelPath is where a channel's head is served.
func ChannelPath(channel release.Channel) string { return "channels/" + string(channel) + ".json" }

// ParseTag reads the label and the channel a tag names.
//
// The tag is the only thing that decides a release's channel: a calendar tag
// carrying a -beta. segment is a beta, any other calendar tag is stable, and an
// edge-<n> tag is an edge build labelled edge.<n>.
func ParseTag(tag string) (string, release.Channel, error) {
	switch {
	case strings.HasPrefix(tag, edgeTagPrefix):
		label := "edge." + strings.TrimPrefix(tag, edgeTagPrefix)
		if err := release.ValidateReleaseLabel(label); err != nil {
			return "", "", err
		}
		return label, release.ChannelEdge, nil

	case strings.HasPrefix(tag, "v"):
		label := strings.TrimPrefix(tag, "v")
		if err := release.ValidateReleaseLabel(label); err != nil {
			return "", "", err
		}
		if strings.HasPrefix(label, "edge.") {
			return "", "", fmt.Errorf("%q labels an edge build, whose tag is %s<n>", tag, edgeTagPrefix)
		}
		if strings.Contains(label, "-beta.") {
			return label, release.ChannelBeta, nil
		}
		return label, release.ChannelStable, nil

	default:
		return "", "", fmt.Errorf("%q is not a release tag", tag)
	}
}

// Generate builds every document a publish run writes.
//
// It returns an error only when a document that passed every check still fails
// to serialise — a defect in this package. Everything a release can get wrong
// is a Skip, so one broken release never stops the rest from being published.
func Generate(releases []Release) (Result, error) {
	candidates, skipped := classify(releases)

	// Publication order, because carry-over looks backwards and a channel head
	// is the most recently published release the channel receives.
	sort.Slice(candidates, func(i, j int) bool {
		return earlier(candidates[i].rel.PublishedAt, candidates[i].label,
			candidates[j].rel.PublishedAt, candidates[j].label)
	})

	var (
		published []*built
		result    Result
	)
	for _, c := range candidates {
		b, err := build(c, published)
		if err != nil {
			skipped = append(skipped, indexedSkip{c.index, Skip{Tag: c.rel.Tag, Reason: err.Error()}})
			continue
		}
		published = append(published, b)
		result.Releases = append(result.Releases, b.document)
	}

	for _, channel := range release.Channels() {
		head := headOf(published, channel)
		if head == nil {
			continue
		}
		doc, err := headDocument(head, channel)
		if err != nil {
			return Result{}, err
		}
		result.Channels = append(result.Channels, doc)
	}

	sort.Slice(skipped, func(i, j int) bool { return skipped[i].index < skipped[j].index })
	for _, s := range skipped {
		result.Skipped = append(result.Skipped, s.skip)
	}
	return result, nil
}

// candidate is a release that named a channel and carried a readable
// release.yaml. Whether its assets are there is decided later, in publication
// order, because a component with no asset of its own may be carried over from
// an earlier release.
type candidate struct {
	rel     Release
	label   string
	channel release.Channel
	spec    releaseYAML
	// index is the position in the input, which is the order skips are
	// reported in.
	index int
}

// built is a published release: its bill of materials and the bytes served for
// it.
type built struct {
	bom      *release.BOM
	document Document
}

// indexedSkip keeps a skip's input position so the report reads in the order
// the caller listed the releases, whatever order they were processed in.
type indexedSkip struct {
	index int
	skip  Skip
}

// classify sorts the input into releases worth building and releases that
// cannot be built at all.
func classify(releases []Release) ([]candidate, []indexedSkip) {
	var (
		candidates []candidate
		skipped    []indexedSkip
		seen       = map[string]string{}
	)
	for i, rel := range releases {
		skip := func(format string, args ...any) {
			skipped = append(skipped, indexedSkip{i, Skip{Tag: rel.Tag, Reason: fmt.Sprintf(format, args...)}})
		}

		if rel.Draft {
			continue
		}
		label, channel, err := ParseTag(rel.Tag)
		if err != nil {
			skip("%v", err)
			continue
		}
		if tag, ok := seen[label]; ok {
			skip("label %s is already published by tag %s", label, tag)
			continue
		}
		if rel.PublishedAt.IsZero() {
			skip("no publication time")
			continue
		}
		// The workflow sets the prerelease flag from the label, so the two
		// disagreeing means one of them does not describe this release — and
		// nothing here can tell which. Publishing anyway risks putting a beta
		// in front of every stable daemon.
		if channel != release.ChannelEdge && rel.Prerelease != (channel == release.ChannelBeta) {
			skip("tag names a %s release, GitHub marks it prerelease=%t", channel, rel.Prerelease)
			continue
		}
		if len(rel.ReleaseYAML) == 0 {
			skip("no release.yaml asset")
			continue
		}
		spec, err := parseReleaseYAML(rel.ReleaseYAML)
		if err != nil {
			skip("release.yaml: %v", err)
			continue
		}
		// An edge build's release.yaml names the calendar label of the commit
		// it was cut from, not the edge label, so only a calendar release's
		// label is asserted against its tag.
		if channel != release.ChannelEdge && spec.label != label {
			skip("release.yaml names release %s, the tag names %s", spec.label, label)
			continue
		}

		seen[label] = rel.Tag
		candidates = append(candidates, candidate{rel: rel, label: label, channel: channel, spec: spec, index: i})
	}
	return candidates, skipped
}

// build turns a candidate into the document published for it, resolving every
// component against this release's assets or against an earlier release that
// published the same component version.
func build(c candidate, earlier []*built) (*built, error) {
	components := make(map[string]release.Component, len(c.spec.components))
	for _, name := range c.spec.componentNames() {
		component, err := resolveComponent(c, name, c.spec.components[name], earlier)
		if err != nil {
			return nil, err
		}
		components[name] = component
	}

	bom := &release.BOM{
		Release:     c.label,
		Channel:     c.channel,
		PublishedAt: c.rel.PublishedAt.UTC(),
		Components:  components,
	}
	body, err := marshal(bom)
	if err != nil {
		return nil, err
	}
	return &built{
		bom: bom,
		document: Document{
			Path:    ReleasePath(c.label),
			Release: c.label,
			Channel: c.channel,
			Bytes:   body,
		},
	}, nil
}

// resolveComponent finds the assets a component's entry points at.
//
// A release that built the component points at its own assets. One that did not
// build it points at the assets of the most recent earlier release publishing
// the same component version, because bytes are never republished under one
// component version.
func resolveComponent(c candidate, name, version string, earlier []*built) (release.Component, error) {
	binary, ok := componentBinaries[name]
	if !ok {
		return release.Component{}, fmt.Errorf("component %s publishes no asset name this generator knows", name)
	}
	version = publishedVersion(version, c.label, c.channel)

	assets := findAssets(c.rel.Assets, binary+"_"+version+"_")
	if len(assets) > 0 {
		return release.Component{Version: version, Assets: assets}, nil
	}

	if carried, from, ok := carryOver(earlier, name, version); ok {
		return release.Component{Version: version, Assets: carried, FromRelease: from}, nil
	}
	return release.Component{}, fmt.Errorf(
		"no %s_%s_<goos>_<goarch> asset, and no earlier release publishes %s %s",
		binary, version, name, version)
}

// publishedVersion is the component version a release's assets are named by.
//
// An edge build's component version carries the run number of the build that
// produced it, so two edge builds of the same commit never claim to be the same
// component version. A release.yaml the edge workflow already stamped carries
// the suffix itself.
func publishedVersion(version, label string, channel release.Channel) string {
	if channel != release.ChannelEdge {
		return version
	}
	suffix := "-edge." + strings.TrimPrefix(label, "edge.")
	if strings.HasSuffix(version, suffix) {
		return version
	}
	return version + suffix
}

// findAssets collects the raw binaries whose names start with prefix, keyed by
// the platform the name ends in.
func findAssets(assets []Asset, prefix string) map[string]release.Asset {
	found := map[string]release.Asset{}
	for _, a := range assets {
		rest, ok := strings.CutPrefix(a.Name, prefix)
		if !ok {
			continue
		}
		goos, goarch, ok := strings.Cut(rest, "_")
		if !ok || !platformPartPattern.MatchString(goos) || !platformPartPattern.MatchString(goarch) {
			continue
		}
		found[release.PlatformKey(goos, goarch)] = release.Asset{URL: a.URL, SHA256: a.SHA256}
	}
	if len(found) == 0 {
		return nil
	}
	return found
}

// carryOver finds the assets an unchanged component keeps pointing at, and the
// release that built them.
//
// The label returned is the release that BUILT the component, not the last one
// to carry it over, so a chain of unchanged releases does not have to be walked
// to reach the bytes.
func carryOver(earlier []*built, name, version string) (map[string]release.Asset, string, bool) {
	for i := len(earlier) - 1; i >= 0; i-- {
		component, ok := earlier[i].bom.Components[name]
		if !ok || component.Version != version {
			continue
		}
		from := component.FromRelease
		if from == "" {
			from = earlier[i].bom.Release
		}
		return component.Assets, from, true
	}
	return nil, "", false
}

// headOf is the release a channel currently offers: the most recently published
// one among the channels it receives.
//
// Channels are cumulative — edge receives what beta and stable publish, beta
// receives what stable publishes — so a release is offered by every channel at
// least as permissive as the one it was cut into. Two releases published at the
// same instant are ordered by label, so a head is a function of the input alone.
func headOf(published []*built, channel release.Channel) *built {
	var head *built
	for _, b := range published {
		if !receives(channel, b.bom.Channel) {
			continue
		}
		if head == nil || earlier(head.bom.PublishedAt, head.bom.Release, b.bom.PublishedAt, b.bom.Release) {
			head = b
		}
	}
	return head
}

// headDocument is the channel head's bill of materials, declaring the channel
// it is served under.
//
// The daemon refuses a document whose channel is not the one it asked for, so a
// stable release offered to edge is re-stated as the edge head rather than
// served with the channel it was cut into.
func headDocument(head *built, channel release.Channel) (Document, error) {
	bom := *head.bom
	bom.Channel = channel
	body, err := marshal(&bom)
	if err != nil {
		return Document{}, fmt.Errorf("%s head: %w", channel, err)
	}
	return Document{
		Path:    ChannelPath(channel),
		Release: bom.Release,
		Channel: channel,
		Bytes:   body,
	}, nil
}

// receives reports whether a channel offers what was cut into another.
func receives(channel, cut release.Channel) bool {
	return permissiveness(channel) >= permissiveness(cut)
}

// permissiveness orders the channels by how much they receive. release.Channels
// lists them most vetted first, so a channel's position in that list is the
// order.
func permissiveness(channel release.Channel) int {
	for i, c := range release.Channels() {
		if c == channel {
			return i
		}
	}
	return -1
}

// earlier orders two releases: by publication time, then by label so that two
// releases published at the same instant still order the same way on every run.
func earlier(at time.Time, label string, otherAt time.Time, otherLabel string) bool {
	if at.Equal(otherAt) {
		return label < otherLabel
	}
	return at.Before(otherAt)
}

// marshal serialises a bill of materials into the bytes that are published and
// signed, and refuses any document its own reader would not accept.
//
// Deterministic by construction: struct fields serialise in declaration order,
// encoding/json sorts map keys, the indentation is fixed and the document ends
// in a newline. Re-running a publish over unchanged releases therefore rewrites
// identical bytes, and a signature made over an earlier run still verifies.
func marshal(bom *release.BOM) ([]byte, error) {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	// URLs are the only place a & or a < could appear, and escaping one would
	// publish a document that reads nothing like the URL it names.
	enc.SetEscapeHTML(false)
	enc.SetIndent("", "  ")
	if err := enc.Encode(bom); err != nil {
		return nil, fmt.Errorf("serialise the bill of materials for %s: %w", bom.Release, err)
	}
	if _, err := release.ParseBOM(buf.Bytes()); err != nil {
		return nil, err
	}
	return buf.Bytes(), nil
}

// releaseYAML is what the release.yaml asset says: the release's label and each
// component's version.
type releaseYAML struct {
	label      string
	components map[string]string
}

// componentNames lists the components in a fixed order, so a document is built
// the same way on every run.
func (r releaseYAML) componentNames() []string {
	names := make([]string, 0, len(r.components))
	for name := range r.components {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

// releaseKeyPattern matches the top-level `release:` key. Top-level, so it
// starts at column 1: an indented one belongs to some other mapping and is not
// the label.
var releaseKeyPattern = regexp.MustCompile(`^release:[ \t]*(.*)$`)

// parseReleaseYAML reads the label and the component versions out of a
// release.yaml.
//
// The parse is the same plain-text one scripts/release-label.sh and
// scripts/release-component-version.sh do, on the same file: a top-level
// `release:` key, and a `components:` block of indented `name: version` entries
// ending at the next line starting in column 1.
func parseReleaseYAML(data []byte) (releaseYAML, error) {
	parsed := releaseYAML{components: map[string]string{}}
	inComponents := false

	for _, raw := range strings.Split(string(data), "\n") {
		line := strings.TrimRight(raw, "\r")
		if line == "" {
			continue
		}
		indented := strings.HasPrefix(line, " ") || strings.HasPrefix(line, "\t")
		if !indented {
			inComponents = strings.HasPrefix(line, "components:")
			if m := releaseKeyPattern.FindStringSubmatch(line); m != nil {
				if parsed.label != "" {
					return releaseYAML{}, fmt.Errorf("two top-level 'release:' keys; exactly one names the label")
				}
				parsed.label = scalar(m[1])
			}
			continue
		}
		if !inComponents {
			continue
		}
		name, version, ok := componentEntry(line)
		if !ok {
			continue
		}
		if _, dup := parsed.components[name]; dup {
			return releaseYAML{}, fmt.Errorf("two entries for component %s; exactly one names its version", name)
		}
		parsed.components[name] = version
	}

	if parsed.label == "" {
		return releaseYAML{}, fmt.Errorf("no top-level 'release:' key")
	}
	if err := release.ValidateReleaseLabel(parsed.label); err != nil {
		return releaseYAML{}, err
	}
	if len(parsed.components) == 0 {
		return releaseYAML{}, fmt.Errorf("no components")
	}
	return parsed, nil
}

// componentEntry reads one `name: version` line of the components block.
func componentEntry(line string) (string, string, bool) {
	entry := strings.TrimSpace(line)
	if entry == "" || strings.HasPrefix(entry, "#") {
		return "", "", false
	}
	name, version, ok := strings.Cut(entry, ":")
	if !ok {
		return "", "", false
	}
	version = scalar(version)
	if version == "" {
		return "", "", false
	}
	return strings.TrimSpace(name), version, true
}

// scalar is a YAML scalar as these two files ever write one: a value, an
// optional inline comment, and quoting that a label or a version never
// contains.
func scalar(value string) string {
	if i := strings.Index(value, "#"); i >= 0 {
		value = value[:i]
	}
	return strings.Trim(strings.TrimSpace(value), `"'`)
}
