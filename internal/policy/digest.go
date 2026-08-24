package policy

import (
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"slices"
	"strconv"
	"strings"
)

// digestVersion prefixes the canonical encoding. Bump it only when the
// ENCODING changes shape; changes to what is encoded are already visible in
// the bytes.
const digestVersion = 1

// digestChars is the length of a PolicyDigest: the first 64 bits of the
// SHA-256 over the canonical encoding, hex-encoded. Short enough to sit in a
// note's frontmatter, wide enough that an accidental collision between two
// policies a single user writes is not a thing that happens.
const digestChars = 16

// buildCanonical renders the effective policy as deterministic, sorted,
// newline-separated text. Everything that can change a Verdict is in here and
// nothing else is: attachments.quarantine changes how a stored file is tagged,
// never whether it is stored, so it is excluded on purpose.
//
// Map iteration order, slice order and the order the operator wrote
// allow_extensions in are all erased by sorting, so two configs that mean the
// same thing hash the same.
func (p *Policy) buildCanonical() string {
	var b strings.Builder
	fmt.Fprintf(&b, "policy/v%d\n", digestVersion)
	fmt.Fprintf(&b, "tolerations=v%d\n", tolerationRulesVersion)

	scan := make([]string, len(p.set.ScanCommand))
	for i, a := range p.set.ScanCommand {
		scan[i] = strconv.Quote(a)
	}

	// Scalars, sorted by key so adding one never reorders the rest.
	scalars := []string{
		"allow_containers=" + strconv.FormatBool(p.set.AllowContainers),
		"allow_macro_office=" + strconv.FormatBool(p.set.AllowMacroOffice),
		"allow_svg=" + strconv.FormatBool(p.set.AllowSVG),
		// The EFFECTIVE chat cap, not the raw field. ChatMaxSize <= 0 means
		// "inherit MaxSize", so a Settings that leaves it zero and one that
		// spells the inherited number out decide every attachment
		// identically and must therefore hash identically.
		//
		// This is not cosmetic. policy.DefaultSettings leaves ChatMaxSize at
		// 0 while config.Attachments resolves it to max_size before building
		// the policy, so before this normalization policy.Default() and
		// cfg.Policy() over a DEFAULT config produced two different digests
		// for one behaviour — and the digest is what the whole retro-fetch
		// protocol keys on. Any archive whose recorded digest came from one
		// and whose running policy came from the other would report "the
		// attachment policy has changed" on every single run, forever.
		"chat_max_size=" + strconv.FormatInt(p.MaxSizeFor(true), 10),
		"free_space_floor=" + strconv.FormatInt(p.set.FreeSpaceFloor, 10),
		"max_message_bytes=" + strconv.FormatInt(p.set.MaxMessageBytes, 10),
		"max_parts_per_message=" + strconv.Itoa(p.set.MaxPartsPerMessage),
		"max_per_message=" + strconv.FormatInt(p.set.MaxPerMessage, 10),
		"max_size=" + strconv.FormatInt(p.set.MaxSize, 10),
		"on_mismatch=" + p.set.OnMismatch,
		"run_budget=" + strconv.FormatInt(p.set.RunBudget, 10),
		"scan_action=" + p.set.ScanAction,
		"scan_command=[" + strings.Join(scan, " ") + "]",
	}
	slices.Sort(scalars)
	for _, s := range scalars {
		b.WriteString(s)
		b.WriteByte('\n')
	}

	// The effective allowlist: one sorted line per extension, types sorted.
	exts := make([]string, 0, len(p.perExt))
	for ext := range p.perExt {
		exts = append(exts, ext)
	}
	slices.Sort(exts)
	for _, ext := range exts {
		types := slices.Clone(p.perExt[ext])
		slices.Sort(types)
		fmt.Fprintf(&b, "ext:%s=%s\n", ext, strings.Join(types, ","))
	}
	return b.String()
}

func digestOf(canonical string) string {
	sum := sha256.Sum256([]byte(canonical))
	return hex.EncodeToString(sum[:])[:digestChars]
}
