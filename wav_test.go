package main

import (
	"encoding/binary"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
)

func makeTestWAV() []byte {
	const sampleBytes = 8
	data := make([]byte, 44+sampleBytes)
	copy(data[0:4], "RIFF")
	binary.LittleEndian.PutUint32(data[4:8], uint32(len(data)-8))
	copy(data[8:12], "WAVE")
	copy(data[12:16], "fmt ")
	binary.LittleEndian.PutUint32(data[16:20], 16)
	binary.LittleEndian.PutUint16(data[20:22], 1)    // PCM
	binary.LittleEndian.PutUint16(data[22:24], 1)    // mono
	binary.LittleEndian.PutUint32(data[24:28], 8000) // sample rate
	binary.LittleEndian.PutUint32(data[28:32], 8000) // byte rate
	binary.LittleEndian.PutUint16(data[32:34], 1)    // block align
	binary.LittleEndian.PutUint16(data[34:36], 8)    // bits per sample
	copy(data[36:40], "data")
	binary.LittleEndian.PutUint32(data[40:44], sampleBytes)
	for index := 44; index < len(data); index++ {
		data[index] = 128
	}
	return data
}

func writeTestWAV(t *testing.T, path string) {
	t.Helper()
	if err := os.WriteFile(path, makeTestWAV(), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestValidateWAVFile(t *testing.T) {
	dir := t.TempDir()
	valid := filepath.Join(dir, "valid.wav")
	writeTestWAV(t, valid)
	if err := validateWAVFile(valid); err != nil {
		t.Fatalf("valid WAV rejected: %v", err)
	}

	upper := filepath.Join(dir, "upper.WAV")
	writeTestWAV(t, upper)
	if err := validateWAVFile(upper); err != nil {
		t.Fatalf("uppercase WAV extension rejected: %v", err)
	}

	cases := []struct {
		name string
		path string
		data []byte
	}{
		{name: "wrong extension", path: filepath.Join(dir, "sound.mp3"), data: makeTestWAV()},
		{name: "not RIFF", path: filepath.Join(dir, "not-riff.wav"), data: []byte("NOTRxxxxxxxx")},
		{name: "missing format chunk", path: filepath.Join(dir, "no-format.wav"), data: makeWAVWithoutChunk("fmt ")},
		{name: "missing data chunk", path: filepath.Join(dir, "no-data.wav"), data: makeWAVWithoutChunk("data")},
		{name: "truncated RIFF", path: filepath.Join(dir, "truncated.wav"), data: []byte("RIFF\x40\x00\x00\x00WAVE")},
	}
	for _, test := range cases {
		t.Run(test.name, func(t *testing.T) {
			if err := os.WriteFile(test.path, test.data, 0600); err != nil {
				t.Fatal(err)
			}
			if err := validateWAVFile(test.path); err == nil {
				t.Fatal("invalid WAV was accepted")
			}
		})
	}

	if err := validateWAVFile(filepath.Join(dir, "missing.wav")); err == nil {
		t.Fatal("missing file was accepted")
	}
	directory := filepath.Join(dir, "directory.wav")
	if err := os.Mkdir(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err := validateWAVFile(directory); err == nil {
		t.Fatal("directory was accepted as a WAV file")
	}
}

func makeWAVWithoutChunk(omitted string) []byte {
	valid := makeTestWAV()
	if omitted == "fmt " {
		// Keep the RIFF/WAVE header and data chunk only.
		data := append([]byte(nil), valid[:12]...)
		data = append(data, valid[36:]...)
		copy(data[4:8], []byte{byte(len(data) - 8), 0, 0, 0})
		return data
	}
	// Keep the RIFF/WAVE header and fmt chunk only.
	data := append([]byte(nil), valid[:36]...)
	copy(data[4:8], []byte{byte(len(data) - 8), 0, 0, 0})
	return data
}

func TestInvalidSoundSelectionWarnsAndFallsBack(t *testing.T) {
	p, out, _ := testPlugin(t, fixedNow())
	fake := &fakeSound{}
	p.sound = fake
	invalid := filepath.Join(t.TempDir(), "not-a-wave.wav")
	if err := os.WriteFile(invalid, []byte("this is not a WAV"), 0600); err != nil {
		t.Fatal(err)
	}

	params, err := json.Marshal(map[string]any{
		"settings": map[string]any{"sound_enabled": true, "sound_file": invalid},
	})
	if err != nil {
		t.Fatal(err)
	}
	line, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "method": "settingsChanged", "params": json.RawMessage(params),
	})
	if err != nil {
		t.Fatal(err)
	}
	p.handleLine(line)
	if got := notificationCount(out.String()); got != 1 {
		t.Fatalf("invalid sound selection notifications=%d want=1; output=%s", got, out.String())
	}

	p.mu.Lock()
	p.playReminderSoundLocked()
	p.mu.Unlock()
	_, defaults, _ := fake.snapshot()
	if defaults != 1 {
		t.Fatalf("invalid WAV should fall back to system sound, defaults=%d", defaults)
	}
}
