package model

import (
	"encoding/binary"
	"encoding/json"
	"testing"
)

func TestBuildStreamingWAVHeader(t *testing.T) {
	h := buildStreamingWAVHeader(24000, 1, 16)
	if len(h) != 44 {
		t.Fatalf("expected 44 bytes, got %d", len(h))
	}
	if string(h[0:4]) != "RIFF" {
		t.Fatalf("expected RIFF magic, got %q", h[0:4])
	}
	if binary.LittleEndian.Uint32(h[4:8]) != 0xFFFFFFFF {
		t.Fatal("expected RIFF size = 0xFFFFFFFF for streaming")
	}
	if string(h[8:12]) != "WAVE" {
		t.Fatalf("expected WAVE, got %q", h[8:12])
	}
	if string(h[12:16]) != "fmt " {
		t.Fatalf("expected fmt , got %q", h[12:16])
	}
	if binary.LittleEndian.Uint16(h[20:22]) != 1 {
		t.Fatal("expected PCM format (1)")
	}
	if binary.LittleEndian.Uint16(h[22:24]) != 1 {
		t.Fatal("expected 1 channel")
	}
	if binary.LittleEndian.Uint32(h[24:28]) != 24000 {
		t.Fatal("expected sample rate 24000")
	}
	if binary.LittleEndian.Uint32(h[28:32]) != 48000 {
		t.Fatal("expected byte rate 48000")
	}
	if string(h[36:40]) != "data" {
		t.Fatalf("expected data chunk, got %q", h[36:40])
	}
	if binary.LittleEndian.Uint32(h[40:44]) != 0xFFFFFFFF {
		t.Fatal("expected data size = 0xFFFFFFFF for streaming")
	}
}

func TestBuildWAV(t *testing.T) {
	pcm := make([]byte, 100)
	for i := range pcm {
		pcm[i] = byte(i)
	}
	wav := buildWAV(pcm, 24000, 1, 16)
	if len(wav) != 44+100 {
		t.Fatalf("expected %d bytes, got %d", 44+100, len(wav))
	}
	riffSize := binary.LittleEndian.Uint32(wav[4:8])
	if riffSize != uint32(36+100) {
		t.Fatalf("expected RIFF size %d, got %d", 36+100, riffSize)
	}
	dataSize := binary.LittleEndian.Uint32(wav[40:44])
	if dataSize != 100 {
		t.Fatalf("expected data size 100, got %d", dataSize)
	}
	for i := 0; i < 100; i++ {
		if wav[44+i] != byte(i) {
			t.Fatalf("PCM data mismatch at byte %d", i)
		}
	}
}

func TestOpenAIChunkDeltaAudioUnmarshal(t *testing.T) {
	data := `{"content":"hello","audio":{"data":"AAAA","transcript":"hi"}}`
	var delta openAIChunkDelta
	if err := json.Unmarshal([]byte(data), &delta); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if delta.Content != "hello" {
		t.Fatalf("expected content 'hello', got %q", delta.Content)
	}
	if delta.Audio == nil {
		t.Fatal("expected audio to be non-nil")
	}
	if delta.Audio.Data != "AAAA" {
		t.Fatalf("expected audio data 'AAAA', got %q", delta.Audio.Data)
	}
	if delta.Audio.Transcript != "hi" {
		t.Fatalf("expected transcript 'hi', got %q", delta.Audio.Transcript)
	}
}

func TestOpenAIChunkDeltaNoAudio(t *testing.T) {
	data := `{"content":"text only"}`
	var delta openAIChunkDelta
	if err := json.Unmarshal([]byte(data), &delta); err != nil {
		t.Fatalf("unmarshal failed: %v", err)
	}
	if delta.Audio != nil {
		t.Fatal("expected audio to be nil")
	}
}
