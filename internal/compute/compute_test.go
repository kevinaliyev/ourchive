package compute

import (
	"slices"
	"testing"
)

func TestSelectH264EncoderAndDiscordPresetArgs(t *testing.T) {
	if got := SelectH264Encoder([]string{"hevc_nvenc", "h264_nvenc"}); got != "h264_nvenc" {
		t.Fatalf("expected h264_nvenc, got %s", got)
	}
	if got := SelectH264Encoder(nil); got != "libx264" {
		t.Fatalf("expected libx264 fallback, got %s", got)
	}

	args, enc := BuildFFmpegArgs("in.mkv", "out.mp4", "mp4", "discord_10mb", []string{"h264_nvenc"}, 30)
	if enc != "h264_nvenc" {
		t.Fatalf("expected h264_nvenc, got %s", enc)
	}
	if !slices.Contains(args, "+faststart") || !slices.Contains(args, "-b:v") {
		t.Fatalf("expected +faststart and bitrate flags in discord_10mb args: %v", args)
	}
}
