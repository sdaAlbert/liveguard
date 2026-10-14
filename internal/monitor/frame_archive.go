package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
)

const maxArchivedFrameBytes = 12 << 20

func (s *Service) archiveFrame(runID, eventID, source string) (string, error) {
	if runID == "" || eventID == "" || source == "" {
		return "", errors.New("run, observation and source frame are required for archive")
	}
	idBytes := sha256.Sum256([]byte(runID + "\x00" + eventID))
	archiveID := hex.EncodeToString(idBytes[:])
	runBytes := sha256.Sum256([]byte(runID))
	directory := filepath.Join(s.frameArchiveDir, hex.EncodeToString(runBytes[:]))
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	input, err := os.Open(source)
	if err != nil {
		return "", err
	}
	defer input.Close()
	info, err := input.Stat()
	if err != nil {
		return "", err
	}
	if !info.Mode().IsRegular() || info.Size() <= 0 || info.Size() > maxArchivedFrameBytes {
		return "", ErrFrameBudget
	}
	destination := filepath.Join(directory, archiveID+".png")
	if _, err := os.Stat(destination); err == nil {
		return archiveID, nil
	}
	temporary, err := os.CreateTemp(directory, ".frame-*.tmp")
	if err != nil {
		return "", err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	written, copyErr := io.Copy(temporary, io.LimitReader(input, maxArchivedFrameBytes+1))
	closeErr := temporary.Close()
	if copyErr != nil || closeErr != nil || written != info.Size() || written > maxArchivedFrameBytes {
		return "", ErrFrameBudget
	}
	if err := os.Rename(temporaryPath, destination); err != nil {
		if _, statErr := os.Stat(destination); statErr == nil {
			return archiveID, nil
		}
		return "", err
	}
	return archiveID, nil
}

func (s *Service) archivedFramePath(runID, archiveID string) (string, error) {
	if len(archiveID) != 64 {
		return "", errors.New("invalid frame archive id")
	}
	if _, err := hex.DecodeString(archiveID); err != nil {
		return "", errors.New("invalid frame archive id")
	}
	runBytes := sha256.Sum256([]byte(runID))
	path := filepath.Join(s.frameArchiveDir, hex.EncodeToString(runBytes[:]), archiveID+".png")
	info, err := os.Stat(path)
	if err != nil || !info.Mode().IsRegular() || info.Size() > maxArchivedFrameBytes {
		return "", errors.New("archived frame is unavailable")
	}
	return path, nil
}

func (s *Service) archivedObservationFrame(ctx context.Context, runID, eventID string) (FrameAsset, error) {
	ledger, ok := s.repository.(RunLedger)
	if !ok {
		return FrameAsset{}, errors.New("repository does not support observation history")
	}
	page, err := ledger.SearchObservations(ctx, runID, ObservationQuery{EventID: strings.TrimSpace(eventID), Limit: 1})
	if err != nil || len(page.Events) == 0 || page.Events[0].FrameArchiveID == "" {
		return FrameAsset{}, errors.New("observation has no archived frame")
	}
	path, err := s.archivedFramePath(runID, page.Events[0].FrameArchiveID)
	if err != nil {
		return FrameAsset{}, err
	}
	return FrameAsset{Path: path, ObservedAt: page.Events[0].ObservedAt, ObservationID: page.Events[0].ID, ArchiveID: page.Events[0].FrameArchiveID}, nil
}
