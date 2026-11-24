// Package store persists jobs as one JSON file each, written atomically.
// Callers serialize access (the daemon holds one lock around all state).
package store

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/killedoxygen/berth/internal/job"
)

type Store struct {
	dir  string
	jobs map[string]*job.Job
}

func Open(stateDir string) (*Store, error) {
	s := &Store{dir: stateDir, jobs: map[string]*job.Job{}}
	for _, d := range []string{s.jobDir(), s.LogDir(), s.RunDir()} {
		if err := os.MkdirAll(d, 0o700); err != nil {
			return nil, err
		}
	}
	entries, err := os.ReadDir(s.jobDir())
	if err != nil {
		return nil, err
	}
	for _, e := range entries {
		if !strings.HasSuffix(e.Name(), ".json") {
			continue
		}
		data, err := os.ReadFile(filepath.Join(s.jobDir(), e.Name()))
		if err != nil {
			return nil, err
		}
		var j job.Job
		if err := json.Unmarshal(data, &j); err != nil {
			// A torn write cannot happen (rename is atomic); anything else is foreign. Keep it aside.
			_ = os.Rename(filepath.Join(s.jobDir(), e.Name()), filepath.Join(s.jobDir(), e.Name()+".corrupt"))
			continue
		}
		s.jobs[j.ID] = &j
	}
	return s, nil
}

func (s *Store) jobDir() string { return filepath.Join(s.dir, "jobs") }

// LogDir holds job output for jobs that are not in passthrough mode.
func (s *Store) LogDir() string { return filepath.Join(s.dir, "logs") }

// RunDir holds supervisor result files.
func (s *Store) RunDir() string { return filepath.Join(s.dir, "run") }

func (s *Store) LogPath(id string) string    { return filepath.Join(s.LogDir(), id+".log") }
func (s *Store) ResultPath(id string) string { return filepath.Join(s.RunDir(), id+".result") }
func (s *Store) SpecPath(id string) string   { return filepath.Join(s.jobDir(), id+".json") }

// Save writes the job durably and records it in memory.
func (s *Store) Save(j *job.Job) error {
	s.jobs[j.ID] = j
	return WriteJSON(s.SpecPath(j.ID), j, 0o600)
}

// WriteJSON writes atomically: temp file, fsync, rename.
func WriteJSON(path string, v any, mode os.FileMode) error {
	data, err := json.MarshalIndent(v, "", " ")
	if err != nil {
		return err
	}
	tmp, err := os.CreateTemp(filepath.Dir(path), ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(data); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Chmod(mode); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Sync(); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmp.Name(), path)
}

func (s *Store) Get(id string) *job.Job { return s.jobs[id] }

// Resolve accepts a full id or a unique prefix of at least 4 characters.
func (s *Store) Resolve(ref string) (*job.Job, error) {
	if j, ok := s.jobs[ref]; ok {
		return j, nil
	}
	if len(ref) < 4 {
		return nil, fmt.Errorf("job id %q is too short; give at least 4 characters", ref)
	}
	var found *job.Job
	for id, j := range s.jobs {
		if strings.HasPrefix(id, ref) {
			if found != nil {
				return nil, fmt.Errorf("job id %q is ambiguous", ref)
			}
			found = j
		}
	}
	if found == nil {
		return nil, fmt.Errorf("no job %q", ref)
	}
	return found, nil
}

// All returns jobs ordered by creation.
func (s *Store) All() []*job.Job {
	out := make([]*job.Job, 0, len(s.jobs))
	for _, j := range s.jobs {
		out = append(out, j)
	}
	sort.Slice(out, func(a, b int) bool { return out[a].Created.Before(out[b].Created) })
	return out
}

func (s *Store) InStates(states ...job.State) []*job.Job {
	var out []*job.Job
	for _, j := range s.All() {
		for _, st := range states {
			if j.State == st {
				out = append(out, j)
				break
			}
		}
	}
	return out
}

// Prune deletes the oldest finished jobs beyond keep, with their logs.
func (s *Store) Prune(keep int) {
	var done []*job.Job
	for _, j := range s.All() {
		if j.State.Terminal() {
			done = append(done, j)
		}
	}
	for i := 0; i < len(done)-keep; i++ {
		id := done[i].ID
		delete(s.jobs, id)
		_ = os.Remove(s.SpecPath(id))
		_ = os.Remove(s.LogPath(id))
		_ = os.Remove(s.ResultPath(id))
	}
}
