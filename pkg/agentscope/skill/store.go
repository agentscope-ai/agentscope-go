package skill

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
)

// Workspace skill layout (port of Python #2283 partition semantics):
//
//	<workspace>/skills/.seed/<skill-dir>/SKILL.md      seed template
//	<workspace>/skills/<agent_id>/<skill-dir>/SKILL.md one partition per agent
//
// An agent's first skill access equips its partition from .seed; the
// partition directory's existence afterwards is the "already equipped"
// marker, so a seed skill the agent later deletes stays deleted.
const (
	// SkillsDirName is the workspace-relative directory holding partitions.
	SkillsDirName = "skills"
	// SeedPartitionName is the template partition new agents equip from.
	SeedPartitionName = ".seed"
	// DefaultPartitionName is the partition for callers naming no agent.
	DefaultPartitionName = "default"
)

// Sentinel errors the HTTP layer maps onto status codes.
var (
	// ErrAlreadyExists signals a skill directory already in the partition.
	ErrAlreadyExists = errors.New("skill: already exists in the partition")
	// ErrNotFound signals no such skill in the partition.
	ErrNotFound = errors.New("skill: not found")
	// ErrInvalidAgentID signals an agent ID unusable as a partition name.
	ErrInvalidAgentID = errors.New("skill: agent id cannot be used as a partition name")
	// ErrInvalidInput signals a malformed Add request (missing name etc.).
	ErrInvalidInput = errors.New("skill: invalid input")
)

// rootLocks serializes skill operations per workspace root across Store
// instances in this process (the app creates a fresh Store per request,
// so a per-Store mutex alone would not keep concurrent first-equips or
// same-name Adds from racing). The map only grows — one small entry per
// workspace root seen — which is acceptable for app workloads.
var (
	rootsMu   sync.Mutex
	rootLocks = map[string]*sync.Mutex{}
)

func lockForRoot(root string) *sync.Mutex {
	rootsMu.Lock()
	defer rootsMu.Unlock()
	lk, ok := rootLocks[root]
	if !ok {
		lk = &sync.Mutex{}
		rootLocks[root] = lk
	}
	return lk
}

// Store manages per-agent skill partitions under one workspace root.
// All operations are on the host filesystem at the workspace's base path.
type Store struct {
	root string // workspace root directory

	mu       sync.Mutex
	equipped map[string]bool
	migrated bool
}

// NewStore creates a skill store rooted at the workspace directory.
func NewStore(workspaceRoot string) *Store {
	return &Store{
		root:     workspaceRoot,
		equipped: make(map[string]bool),
	}
}

// validateAgentID mirrors Python's _skill_partition guard: a leading dot
// collides with .seed (and "." / ".."), separators are the escape.
func validateAgentID(agentID string) error {
	if agentID == "" {
		return nil // maps to the default partition
	}
	if strings.HasPrefix(agentID, ".") ||
		strings.Contains(agentID, "/") ||
		strings.Contains(agentID, "\\") {
		return fmt.Errorf("%w: %q", ErrInvalidAgentID, agentID)
	}
	return nil
}

// PartitionPath returns the partition directory for an agent ("" maps to
// the default partition). It does not create anything.
func (s *Store) PartitionPath(agentID string) string {
	if agentID == "" {
		agentID = DefaultPartitionName
	}
	return filepath.Join(s.root, SkillsDirName, agentID)
}

// skillsDir returns the top-level skills directory.
func (s *Store) skillsDir() string {
	return filepath.Join(s.root, SkillsDirName)
}

// MigrateLegacy moves pre-partition skills into the seed template.
//
// The legacy layout put skill directories directly under skills/, plus a
// .skills index file. A top-level directory that holds a SKILL.md of its
// own is a legacy skill; the .skills file becomes the seed's .index;
// anything else is a partition and stays put. The move is idempotent: a
// second run finds nothing to move. When the seed already holds an entry
// of the same name, the leftover is kept rather than clobbering it.
func (s *Store) MigrateLegacy() error {
	lk := lockForRoot(s.root)
	lk.Lock()
	defer lk.Unlock()
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.migrateLocked()
}

func (s *Store) migrateLocked() error {
	if s.migrated {
		return nil
	}
	entries, err := os.ReadDir(s.skillsDir())
	if err != nil {
		if os.IsNotExist(err) {
			s.migrated = true
			return nil
		}
		return fmt.Errorf("skill: read skills dir: %w", err)
	}
	type move struct{ src, dst string }
	var moves []move
	for _, e := range entries {
		if e.Name() == SeedPartitionName {
			continue
		}
		path := filepath.Join(s.skillsDir(), e.Name())
		if e.Name() == ".skills" && !e.IsDir() {
			// Legacy index file (Python parity).
			moves = append(moves, move{path, filepath.Join(s.skillsDir(), SeedPartitionName, ".index")})
			continue
		}
		if !e.IsDir() {
			continue
		}
		if _, err := os.Stat(filepath.Join(path, "SKILL.md")); err != nil {
			continue // a partition, not a legacy skill
		}
		moves = append(moves, move{path, filepath.Join(s.skillsDir(), SeedPartitionName, e.Name())})
	}
	for _, m := range moves {
		if _, err := os.Stat(m.dst); err == nil {
			continue // seed copy already present — keep it
		}
		if err := os.MkdirAll(filepath.Join(s.skillsDir(), SeedPartitionName), 0o755); err != nil {
			return fmt.Errorf("skill: create seed dir: %w", err)
		}
		if err := os.Rename(m.src, m.dst); err != nil {
			return fmt.Errorf("skill: migrate %q to seed: %w", filepath.Base(m.src), err)
		}
	}
	s.migrated = true
	return nil
}

// uniqueStagingSuffix makes equip staging names unique so concurrent
// first-touches of a partition cannot destroy each other's in-flight
// copies (Python parity: partition + '.equipping-' + pid).
func uniqueStagingSuffix() string {
	var b [4]byte
	suffix := strconv.Itoa(os.Getpid())
	if _, err := rand.Read(b[:]); err == nil {
		suffix += "-" + hex.EncodeToString(b[:])
	}
	return suffix
}

// equip returns the agent's partition, creating it on first sight by
// copying the seed template (staged, then renamed into place; a rename
// loser drops its copy and accepts the winner's partition).
func (s *Store) equip(agentID string) (string, error) {
	if err := validateAgentID(agentID); err != nil {
		return "", err
	}
	partition := s.PartitionPath(agentID)

	s.mu.Lock()
	defer s.mu.Unlock()
	if err := s.migrateLocked(); err != nil {
		return "", err
	}
	if s.equipped[partition] {
		return partition, nil
	}
	if _, err := os.Stat(partition); err == nil {
		s.equipped[partition] = true
		return partition, nil
	}

	if err := os.MkdirAll(filepath.Dir(partition), 0o755); err != nil {
		return "", fmt.Errorf("skill: create skills dir: %w", err)
	}
	staging := partition + ".equipping-" + uniqueStagingSuffix()
	seed := filepath.Join(s.skillsDir(), SeedPartitionName)
	if info, err := os.Stat(seed); err == nil && info.IsDir() {
		if err := copyDir(seed, staging); err != nil {
			_ = os.RemoveAll(staging)
			return "", fmt.Errorf("skill: equip partition from seed: %w", err)
		}
	} else if err := os.MkdirAll(staging, 0o755); err != nil {
		return "", fmt.Errorf("skill: create partition: %w", err)
	}
	if err := os.Rename(staging, partition); err != nil {
		_ = os.RemoveAll(staging)
		// A concurrent first-touch won the rename; their partition is
		// the shared outcome (Python parity: the loser drops its copy).
		if _, statErr := os.Stat(partition); statErr == nil {
			s.equipped[partition] = true
			return partition, nil
		}
		return "", fmt.Errorf("skill: equip partition: %w", err)
	}
	s.equipped[partition] = true
	return partition, nil
}

// List returns the skills in the agent's partition, equipping it from the
// seed template on first sight. Only <partition>/<dir>/SKILL.md counts —
// a SKILL.md a skill ships in a subfolder is not a second skill (Python
// parity).
func (s *Store) List(agentID string) ([]Skill, error) {
	lk := lockForRoot(s.root)
	lk.Lock()
	defer lk.Unlock()

	partition, err := s.equip(agentID)
	if err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(partition)
	if err != nil {
		if os.IsNotExist(err) {
			return nil, nil
		}
		return nil, fmt.Errorf("skill: read partition: %w", err)
	}
	var skills []Skill
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(partition, e.Name())
		sk, err := parseSKILLFile(filepath.Join(dir, "SKILL.md"), dir)
		if err != nil {
			continue // unparseable entries are skipped, matching Python
		}
		skills = append(skills, *sk)
	}
	sort.Slice(skills, func(i, j int) bool { return skills[i].Name < skills[j].Name })
	return skills, nil
}

// AddDir copies a local skill directory into the agent's partition. The
// source must contain a SKILL.md; a directory with the same basename
// already in the partition is an error (Python parity).
func (s *Store) AddDir(agentID, srcDir string) error {
	lk := lockForRoot(s.root)
	lk.Lock()
	defer lk.Unlock()

	partition, err := s.equip(agentID)
	if err != nil {
		return err
	}
	absSrc, err := filepath.Abs(srcDir)
	if err != nil {
		return fmt.Errorf("skill: resolve source: %w", err)
	}
	if _, err := os.Stat(filepath.Join(absSrc, "SKILL.md")); err != nil {
		return fmt.Errorf("skill: invalid skill at %q: SKILL.md not found", srcDir)
	}
	dirName := filepath.Base(absSrc)
	if !validSkillDirName(dirName) {
		return fmt.Errorf("skill: invalid skill directory name %q", dirName)
	}
	dest := filepath.Join(partition, dirName)
	if _, err := os.Stat(dest); err == nil {
		return fmt.Errorf("directory %q %w", dirName, ErrAlreadyExists)
	}
	if err := copyDir(absSrc, dest); err != nil {
		_ = os.RemoveAll(dest)
		return fmt.Errorf("skill: copy skill %q: %w", dirName, err)
	}
	return nil
}

// Add writes a skill from content into the agent's partition: a directory
// named after the skill with a SKILL.md carrying the frontmatter and body.
// It returns the directory name used.
func (s *Store) Add(agentID, name, description, category, markdown string) (string, error) {
	if strings.TrimSpace(name) == "" {
		return "", fmt.Errorf("%w: name is required", ErrInvalidInput)
	}
	if strings.TrimSpace(markdown) == "" {
		return "", fmt.Errorf("%w: instructions are required", ErrInvalidInput)
	}
	lk := lockForRoot(s.root)
	lk.Lock()
	defer lk.Unlock()

	partition, err := s.equip(agentID)
	if err != nil {
		return "", err
	}
	dirName := SkillDirName(name)
	dest := filepath.Join(partition, dirName)
	if _, err := os.Stat(dest); err == nil {
		return "", fmt.Errorf("directory %q %w", dirName, ErrAlreadyExists)
	}
	if description == "" {
		description = name
	}
	data, err := renderSKILLMD(name, description, category, markdown)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(dest, 0o755); err != nil {
		return "", fmt.Errorf("skill: create skill dir: %w", err)
	}
	if err := os.WriteFile(filepath.Join(dest, "SKILL.md"), data, 0o644); err != nil {
		_ = os.RemoveAll(dest)
		return "", fmt.Errorf("skill: write SKILL.md: %w", err)
	}
	return dirName, nil
}

// Remove deletes the skill whose frontmatter name matches from the agent's
// partition (Python parity: look up via list, remove the directory).
func (s *Store) Remove(agentID, skillName string) error {
	lk := lockForRoot(s.root)
	lk.Lock()
	defer lk.Unlock()

	partition, err := s.equip(agentID)
	if err != nil {
		return err
	}
	entries, err := os.ReadDir(partition)
	if err != nil {
		return fmt.Errorf("skill: read partition: %w", err)
	}
	for _, e := range entries {
		if !e.IsDir() {
			continue
		}
		dir := filepath.Join(partition, e.Name())
		sk, err := parseSKILLFile(filepath.Join(dir, "SKILL.md"), dir)
		if err != nil || sk.Name != skillName {
			continue
		}
		if err := os.RemoveAll(dir); err != nil {
			return fmt.Errorf("skill: remove %q: %w", skillName, err)
		}
		return nil
	}
	return fmt.Errorf("%w: %q in partition %q", ErrNotFound, skillName, partitionLabel(agentID))
}

// PurgeAgent removes the agent's whole partition (e.g. when the agent is
// deleted). The next access re-equips from the seed.
func (s *Store) PurgeAgent(agentID string) error {
	if err := validateAgentID(agentID); err != nil {
		return err
	}
	partition := s.PartitionPath(agentID)
	lk := lockForRoot(s.root)
	lk.Lock()
	defer lk.Unlock()
	s.mu.Lock()
	delete(s.equipped, partition)
	s.mu.Unlock()
	if err := os.RemoveAll(partition); err != nil {
		return fmt.Errorf("skill: purge partition: %w", err)
	}
	return nil
}

func partitionLabel(agentID string) string {
	if agentID == "" {
		return DefaultPartitionName
	}
	return agentID
}

// SkillDirName turns a skill name into a safe directory name.
func SkillDirName(name string) string {
	var b strings.Builder
	for _, r := range strings.ToLower(strings.TrimSpace(name)) {
		switch {
		case r >= 'a' && r <= 'z', r >= '0' && r <= '9', r == '-', r == '_', r == '.':
			b.WriteRune(r)
		default:
			b.WriteRune('-')
		}
	}
	out := strings.Trim(b.String(), "-.")
	if out == "" {
		out = "skill"
	}
	return out
}

// validSkillDirName guards directory basenames taken from user input
// (AddDir sources) against escaping the partition.
func validSkillDirName(name string) bool {
	if name == "" || name == "." || name == ".." || strings.HasPrefix(name, ".") {
		return false
	}
	return !strings.Contains(name, "/") && !strings.Contains(name, "\\")
}

// parseSKILLFile reads and parses one SKILL.md.
func parseSKILLFile(path, dir string) (*Skill, error) {
	info, err := os.Stat(path)
	if err != nil {
		return nil, err
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	mtime := float64(info.ModTime().UnixNano()) / 1e9
	return parseSKILLMD(data, dir, mtime)
}

// renderSKILLMD renders a SKILL.md document with YAML frontmatter.
func renderSKILLMD(name, description, category, markdown string) ([]byte, error) {
	var b strings.Builder
	b.WriteString("---\n")
	b.WriteString("name: " + yamlQuote(name) + "\n")
	b.WriteString("description: " + yamlQuote(description) + "\n")
	if category != "" {
		b.WriteString("category: " + yamlQuote(category) + "\n")
	}
	b.WriteString("---\n\n")
	b.WriteString(strings.TrimSpace(markdown))
	b.WriteString("\n")
	return []byte(b.String()), nil
}

// yamlQuote quotes a string for safe inline YAML (double-quoted style).
// Every C0 control is escaped (\n/\r/\t explicitly, the rest as
// \u00XX) — raw controls are illegal in YAML double-quoted scalars.
func yamlQuote(s string) string {
	var b strings.Builder
	b.WriteByte('"')
	for _, r := range s {
		switch r {
		case '\\':
			b.WriteString(`\\`)
		case '"':
			b.WriteString(`\"`)
		case '\n':
			b.WriteString(`\n`)
		case '\r':
			b.WriteString(`\r`)
		case '\t':
			b.WriteString(`\t`)
		default:
			if r < 0x20 {
				b.WriteString(fmt.Sprintf(`\u%04x`, r))
			} else {
				b.WriteRune(r)
			}
		}
	}
	b.WriteByte('"')
	return b.String()
}

// copyDir recursively copies src into dst (created fresh).
func copyDir(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("%s is not a directory", src)
	}
	if err := os.MkdirAll(dst, info.Mode().Perm()|0o700); err != nil {
		return err
	}
	entries, err := os.ReadDir(src)
	if err != nil {
		return err
	}
	for _, e := range entries {
		srcPath := filepath.Join(src, e.Name())
		dstPath := filepath.Join(dst, e.Name())
		if e.IsDir() {
			if err := copyDir(srcPath, dstPath); err != nil {
				return err
			}
			continue
		}
		if e.Type()&os.ModeSymlink != 0 {
			continue // never follow or copy symlinks
		}
		if err := copyFile(srcPath, dstPath); err != nil {
			return err
		}
	}
	return nil
}

func copyFile(src, dst string) error {
	info, err := os.Stat(src)
	if err != nil {
		return err
	}
	in, err := os.Open(src)
	if err != nil {
		return err
	}
	defer func() { _ = in.Close() }()
	out, err := os.OpenFile(dst, os.O_CREATE|os.O_WRONLY|os.O_TRUNC, info.Mode().Perm())
	if err != nil {
		return err
	}
	if _, err := io.Copy(out, in); err != nil {
		_ = out.Close()
		return err
	}
	return out.Close()
}
