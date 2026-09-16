// Copyright 2026 Dolthub, Inc.
//
// Licensed under the Apache License, Version 2.0 (the "License");
// you may not use this file except in compliance with the License.
// You may obtain a copy of the License at
//
//     http://www.apache.org/licenses/LICENSE-2.0
//
// Unless required by applicable law or agreed to in writing, software
// distributed under the License is distributed on an "AS IS" BASIS,
// WITHOUT WARRANTIES OR CONDITIONS OF ANY KIND, either express or implied.
// See the License for the specific language governing permissions and
// limitations under the License.

package control

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
)

const commitLogNameFormat = "commit-intervals-%020d.jsonl"

type commitLogRecord struct {
	CommitInterval
	Checkpoint *Checkpoint `json:"checkpoint,omitempty"`
}

func (s *Store) commitLogPath(generation uint64) string {
	return filepath.Join(filepath.Dir(s.path), fmt.Sprintf(commitLogNameFormat, generation))
}

func (s *Store) appendCommitIntervalLocked(interval CommitInterval, checkpoint *Checkpoint) error {
	record := commitLogRecord{CommitInterval: interval, Checkpoint: checkpoint}
	data, err := json.Marshal(record)
	if err != nil {
		return fmt.Errorf("encoding commit interval: %w", err)
	}
	data = append(data, '\n')
	path := s.commitLogPath(s.state.CommitLogGeneration)
	file, err := os.OpenFile(path, os.O_WRONLY|os.O_APPEND, 0o600)
	if err != nil {
		return fmt.Errorf("opening commit interval log: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return fmt.Errorf("reading commit interval log size: %w", err)
	}
	originalSize := info.Size()
	if _, err := file.Write(data); err != nil {
		rollbackCommitAppend(file, originalSize)
		return fmt.Errorf("appending commit interval: %w", err)
	}
	if err := file.Sync(); err != nil {
		rollbackCommitAppend(file, originalSize)
		return fmt.Errorf("syncing commit interval log: %w", err)
	}
	if err := file.Close(); err != nil {
		if truncateErr := truncateAndSync(path, originalSize); truncateErr != nil {
			return fmt.Errorf("closing commit interval log: %w; rollback failed: %v", err, truncateErr)
		}
		return fmt.Errorf("closing commit interval log: %w", err)
	}
	return nil
}

func rollbackCommitAppend(file *os.File, size int64) {
	_ = file.Truncate(size)
	_ = file.Sync()
	_ = file.Close()
}

func truncateAndSync(path string, size int64) error {
	file, err := os.OpenFile(path, os.O_WRONLY, 0)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := file.Truncate(size); err != nil {
		return err
	}
	return file.Sync()
}

func (s *Store) loadCommitLogLocked(generation uint64) ([]CommitInterval, *Checkpoint, error) {
	path := s.commitLogPath(generation)
	file, err := os.OpenFile(path, os.O_RDWR, 0)
	if err != nil {
		return nil, nil, fmt.Errorf("opening commit interval log generation %d: %w", generation, err)
	}
	defer file.Close()

	var intervals []CommitInterval
	var publishedCheckpoint *Checkpoint
	var completeBytes int64
	reader := bufio.NewReader(file)
	for {
		line, readErr := reader.ReadBytes('\n')
		if readErr != nil {
			if !errors.Is(readErr, io.EOF) {
				return nil, nil, fmt.Errorf("reading commit interval log: %w", readErr)
			}
			if len(line) != 0 {
				if err := file.Truncate(completeBytes); err != nil {
					return nil, nil, fmt.Errorf("discarding incomplete commit interval: %w", err)
				}
				if err := file.Sync(); err != nil {
					return nil, nil, fmt.Errorf("syncing repaired commit interval log: %w", err)
				}
			}
			break
		}
		completeBytes += int64(len(line))
		var record commitLogRecord
		if err := json.Unmarshal(bytes.TrimSuffix(line, []byte{'\n'}), &record); err != nil {
			return nil, nil, fmt.Errorf("decoding commit interval %d: %w", len(intervals), err)
		}
		if record.Checkpoint != nil {
			if err := validatePublishedCheckpoint(record.CommitInterval, *record.Checkpoint); err != nil {
				return nil, nil, fmt.Errorf("validating commit interval %d checkpoint: %w", len(intervals), err)
			}
			if publishedCheckpoint != nil && checkpointRegresses(*record.Checkpoint, *publishedCheckpoint) {
				return nil, nil, fmt.Errorf("commit interval %d checkpoint regresses", len(intervals))
			}
			checkpoint := *record.Checkpoint
			publishedCheckpoint = &checkpoint
		}
		intervals = append(intervals, record.CommitInterval)
	}
	if err := validateCommitIntervals(intervals); err != nil {
		return nil, nil, fmt.Errorf("validating commit interval log: %w", err)
	}
	return intervals, publishedCheckpoint, nil
}

func validateCommitIntervals(intervals []CommitInterval) error {
	commitIDs := make(map[string]struct{}, len(intervals))
	for index, interval := range intervals {
		if interval.CommitID == "" {
			return fmt.Errorf("commit interval %d has no commit ID", index)
		}
		if interval.First.Compare(interval.Last) > 0 {
			return fmt.Errorf("commit interval %d starts after it ends", index)
		}
		if _, ok := commitIDs[interval.CommitID]; ok {
			return fmt.Errorf("commit interval %d repeats commit ID %q", index, interval.CommitID)
		}
		commitIDs[interval.CommitID] = struct{}{}
		for commitIndex, commit := range interval.Commits {
			if commit.Database == "" || commit.CommitID == "" {
				return fmt.Errorf("commit interval %d has incomplete database commit %d", index, commitIndex)
			}
			if commitIndex > 0 && interval.Commits[commitIndex-1].Database >= commit.Database {
				return fmt.Errorf("commit interval %d database commits are not strictly sorted", index)
			}
		}
		if index > 0 && intervals[index-1].Last.Compare(interval.First) >= 0 {
			return fmt.Errorf("commit interval %d overlaps or precedes commit %q", index, intervals[index-1].CommitID)
		}
	}
	return nil
}

func validatePublishedCheckpoint(interval CommitInterval, checkpoint Checkpoint) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if checkpoint.Written != interval.Last || checkpoint.Durable != interval.Last || checkpoint.Applied != interval.Last {
		return errors.New("checkpoint does not publish the complete commit interval")
	}
	return nil
}

func (s *Store) replaceCommitLogLocked(generation uint64, intervals []CommitInterval) error {
	if generation == 0 {
		return errors.New("commit interval log generation must be positive")
	}
	if err := validateCommitIntervals(intervals); err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	temporary, err := os.CreateTemp(dir, ".commit-intervals-*")
	if err != nil {
		return fmt.Errorf("creating commit interval log: %w", err)
	}
	temporaryName := temporary.Name()
	defer os.Remove(temporaryName)
	encoder := json.NewEncoder(temporary)
	for _, interval := range intervals {
		if err := encoder.Encode(commitLogRecord{CommitInterval: interval}); err != nil {
			temporary.Close()
			return fmt.Errorf("writing commit interval log: %w", err)
		}
	}
	if err := temporary.Chmod(0o600); err != nil {
		temporary.Close()
		return fmt.Errorf("securing commit interval log: %w", err)
	}
	if err := temporary.Sync(); err != nil {
		temporary.Close()
		return fmt.Errorf("syncing commit interval log: %w", err)
	}
	if err := temporary.Close(); err != nil {
		return fmt.Errorf("closing commit interval log: %w", err)
	}
	if err := os.Rename(temporaryName, s.commitLogPath(generation)); err != nil {
		return fmt.Errorf("publishing commit interval log: %w", err)
	}
	return syncControlDirectory(dir)
}

func (s *Store) removeCommitLogLocked(generation uint64) {
	if generation == 0 {
		return
	}
	if err := os.Remove(s.commitLogPath(generation)); err != nil && !errors.Is(err, os.ErrNotExist) {
		return
	}
	_ = syncControlDirectory(filepath.Dir(s.path))
}

func syncControlDirectory(dir string) error {
	directory, err := os.Open(dir)
	if err != nil {
		return fmt.Errorf("opening replication control directory: %w", err)
	}
	defer directory.Close()
	if err := directory.Sync(); err != nil {
		return fmt.Errorf("syncing replication control directory: %w", err)
	}
	return nil
}
