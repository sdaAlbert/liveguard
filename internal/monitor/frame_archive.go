package monitor

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
)

const maxArchivedFrameBytes = 12 << 20

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
