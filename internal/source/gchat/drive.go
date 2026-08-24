package gchat

// drive.go mirrors DRIVE_FILE attachments via the Drive API when
// mirror_drive_files is on. Drive rows share the attachments table and its
// retry machinery with uploaded content; only the fetch differs — plain
// binaries download via files.get alt=media, Google-native types export to
// their Office format (see driveExport in mapping.go).

import (
	"context"
	"errors"
	"fmt"
	"io"
	"strings"

	drive "google.golang.org/api/drive/v3"
)

// driveAPI is the thin seam between the connector and
// google.golang.org/api/drive/v3 — just enough surface for tests to fake.
type driveAPI interface {
	getFileMimeType(ctx context.Context, fileID string) (string, error)
	downloadFile(ctx context.Context, fileID string) (io.ReadCloser, error)
	exportFile(ctx context.Context, fileID, mimeType string) (io.ReadCloser, error)
}

type realDriveAPI struct {
	svc *drive.Service
}

func (r *realDriveAPI) getFileMimeType(ctx context.Context, fileID string) (string, error) {
	f, err := r.svc.Files.Get(fileID).Fields("mimeType").SupportsAllDrives(true).Context(ctx).Do()
	if err != nil {
		return "", err
	}
	return f.MimeType, nil
}

func (r *realDriveAPI) downloadFile(ctx context.Context, fileID string) (io.ReadCloser, error) {
	resp, err := r.svc.Files.Get(fileID).SupportsAllDrives(true).Context(ctx).Download()
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

func (r *realDriveAPI) exportFile(ctx context.Context, fileID, mimeType string) (io.ReadCloser, error) {
	resp, err := r.svc.Files.Export(fileID, mimeType).Context(ctx).Download()
	if err != nil {
		return nil, err
	}
	return resp.Body, nil
}

// errDriveNotExportable marks Google-native Drive types with no binary
// export (forms, sites, folders, …): deterministic per file, so the row
// fails permanently instead of burning retry attempts.
var errDriveNotExportable = errors.New("gchat: drive file type has no exportable content")

// fetchDriveFile fetches one Drive file's bytes: metadata first for the MIME
// type, then export for Google-native formats or alt=media for everything
// else.
func (c *Connector) fetchDriveFile(ctx context.Context, fileID string) ([]byte, error) {
	mimeType, err := c.drive.getFileMimeType(ctx, fileID)
	if err != nil {
		return nil, err
	}
	if exportMime, _, ok := driveExport(mimeType); ok {
		return readAll(c.drive.exportFile(ctx, fileID, exportMime))
	}
	if strings.HasPrefix(mimeType, "application/vnd.google-apps.") {
		return nil, fmt.Errorf("%w: %s", errDriveNotExportable, mimeType)
	}
	return readAll(c.drive.downloadFile(ctx, fileID))
}

// readAll drains and closes rc, passing a fetch error through unchanged.
func readAll(rc io.ReadCloser, err error) ([]byte, error) {
	if err != nil {
		return nil, err
	}
	defer rc.Close()
	return io.ReadAll(rc)
}
