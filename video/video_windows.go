//go:build windows

// Package video captures the Windows desktop. The intended backend is the
// FFmpeg ddagrab (Direct3D 11 Desktop Duplication) input device paired with
// a hardware H264 encoder (h264_nvenc on NVIDIA, or the D3D11VA-based
// encoders), mirroring the Linux kmsgrab + h264_vaapi pipeline.
//
// This file is the Windows backend. The cross-platform Streamer interface,
// Config, and New constructor live in video.go; the shared encoder/filter
// logic lives in encoder.go.
//
// NOTE: the ddagrab capture pipeline is not implemented yet. On Windows the
// server always uses ddagrab regardless of --video-source (see main.go), and
// newStreamer below returns a "not yet implemented" error so the server
// degrades gracefully: no video track is added and trackpad/tablet keep
// working. The real ddagrab + encoder pipeline will land in a follow-up
// change; until then this stub keeps the Windows build path structurally
// complete.
package video

import (
	"errors"
	"fmt"
)

// newStreamer is the platform-specific dispatch called by the cross-platform
// New in video.go. On Windows the source is always ddagrab (the only Windows
// capture backend); --video-source is ignored.
func newStreamer(cfg Config) (Streamer, error) {
	return newDdagrabStreamer(cfg)
}

// newDdagrabStreamer is a placeholder for the ddagrab + hardware-encoder
// pipeline. It always fails so callers degrade to running without video.
func newDdagrabStreamer(cfg Config) (Streamer, error) {
	return nil, fmt.Errorf("video: ddagrab backend not yet implemented: %w", errors.ErrUnsupported)
}
