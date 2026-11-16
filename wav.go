package main

import (
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// validateWAVFile checks both the filename extension and the RIFF/WAVE
// container structure. An extension alone is not enough: arbitrary files can
// be renamed to .wav and would otherwise be passed to PlaySoundW.
func validateWAVFile(path string) error {
	path = strings.TrimSpace(path)
	if path == "" {
		return errors.New("no WAV file selected")
	}
	if !strings.EqualFold(filepath.Ext(path), ".wav") {
		return errors.New("file extension is not .wav")
	}

	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open WAV file: %w", err)
	}
	defer file.Close()

	info, err := file.Stat()
	if err != nil {
		return fmt.Errorf("stat WAV file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return errors.New("WAV path is not a regular file")
	}
	if info.Size() < 12 {
		return errors.New("WAV file is shorter than a RIFF header")
	}

	var header [12]byte
	if _, err := file.ReadAt(header[:], 0); err != nil {
		return fmt.Errorf("read RIFF header: %w", err)
	}
	if string(header[0:4]) != "RIFF" || string(header[8:12]) != "WAVE" {
		return errors.New("file is not a RIFF/WAVE container")
	}

	riffSize := int64(binary.LittleEndian.Uint32(header[4:8]))
	if riffSize < 4 {
		return errors.New("invalid RIFF size")
	}
	riffEnd := riffSize + 8
	if riffEnd > info.Size() {
		return errors.New("RIFF data is truncated")
	}

	hasFormat := false
	hasData := false
	for offset := int64(12); offset < riffEnd; {
		if riffEnd-offset < 8 {
			return errors.New("truncated WAV chunk header")
		}

		var chunkHeader [8]byte
		if _, err := file.ReadAt(chunkHeader[:], offset); err != nil {
			return fmt.Errorf("read WAV chunk header: %w", err)
		}
		chunkID := string(chunkHeader[0:4])
		chunkSize := int64(binary.LittleEndian.Uint32(chunkHeader[4:8]))
		chunkStart := offset + 8
		chunkEnd := chunkStart + chunkSize
		paddedEnd := chunkEnd + (chunkSize & 1)
		if chunkEnd < chunkStart || paddedEnd > riffEnd {
			return fmt.Errorf("WAV chunk %q exceeds RIFF bounds", chunkID)
		}

		switch chunkID {
		case "fmt ":
			if chunkSize < 16 {
				return errors.New("WAV fmt chunk is shorter than 16 bytes")
			}
			var format [16]byte
			if _, err := file.ReadAt(format[:], chunkStart); err != nil {
				return fmt.Errorf("read WAV format chunk: %w", err)
			}
			formatTag := binary.LittleEndian.Uint16(format[0:2])
			channels := binary.LittleEndian.Uint16(format[2:4])
			sampleRate := binary.LittleEndian.Uint32(format[4:8])
			blockAlign := binary.LittleEndian.Uint16(format[12:14])
			bitsPerSample := binary.LittleEndian.Uint16(format[14:16])
			if formatTag == 0 || channels == 0 || sampleRate == 0 || blockAlign == 0 || bitsPerSample == 0 {
				return errors.New("WAV fmt chunk contains invalid audio parameters")
			}
			hasFormat = true
		case "data":
			if chunkSize > 0 {
				hasData = true
			}
		}

		offset = paddedEnd
	}

	if !hasFormat {
		return errors.New("WAV file has no valid fmt chunk")
	}
	if !hasData {
		return errors.New("WAV file has no audio data")
	}
	return nil
}
