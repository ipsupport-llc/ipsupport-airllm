package speechcache

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/redis/go-redis/v9"
)

// testRedis connects to TEST_REDIS_URL or skips. Point it at the dev compose
// stack's Redis (make compose-up): redis://127.0.0.1:56379/0.
func testRedis(t *testing.T) *redis.Client {
	t.Helper()
	dsn := os.Getenv("TEST_REDIS_URL")
	if dsn == "" {
		t.Skip("TEST_REDIS_URL not set; skipping synthesis cache Redis test")
	}
	opt, err := redis.ParseURL(dsn)
	if err != nil {
		t.Fatalf("parse redis url: %v", err)
	}
	rdb := redis.NewClient(opt)
	t.Cleanup(func() { _ = rdb.Close() })
	return rdb
}

func TestClipsRoundTripThroughRedisWithTheirTTL(t *testing.T) {
	rdb := testRedis(t)
	c := New(rdb)
	ctx := context.Background()
	k := Key{Alias: "voice-tts-" + time.Now().Format("150405.000000000"), Provider: "gtts", Model: "chirp3-hd",
		Voice: "en-US-Chirp3-HD-Charon", Language: "en-US", Format: "wav", Text: "Hello\nthere"}
	t.Cleanup(func() { _ = rdb.Del(ctx, k.id()).Err() })

	if _, ok, err := c.Get(ctx, k); ok || err != nil {
		t.Fatalf("before Put: ok=%v err=%v, want a clean miss", ok, err)
	}
	want := Clip{Audio: []byte("RIFF\n\x00binary\nWAVE"), ContentType: "audio/wav", Model: "chirp3-hd", Label: "cloud-tts"}
	if err := c.Put(ctx, k, want, 3*time.Hour); err != nil {
		t.Fatalf("Put: %v", err)
	}
	got, ok, err := c.Get(ctx, k)
	if !ok || err != nil || !bytes.Equal(got.Audio, want.Audio) || got.ContentType != want.ContentType || got.Model != want.Model || got.Label != want.Label {
		t.Errorf("Get = %+v ok=%v err=%v, want %+v", got, ok, err, want)
	}
	if ttl := rdb.PTTL(ctx, k.id()).Val(); ttl <= 2*time.Hour || ttl > 3*time.Hour {
		t.Errorf("stored TTL = %v, want about 3h", ttl)
	}
	other := k
	other.Voice = "en-US-Neural2-D"
	if _, ok, _ := c.Get(ctx, other); ok {
		t.Error("another voice's key found the clip")
	}
}

func TestAnUnreachableRedisIsAMissAndIsRestedAfterTheFirstError(t *testing.T) {
	rdb := redis.NewClient(&redis.Options{Addr: "127.0.0.1:1", MaxRetries: -1})
	t.Cleanup(func() { _ = rdb.Close() })
	c := New(rdb)
	ctx := context.Background()
	k := Key{Alias: "a", Text: "hi"}

	if _, ok, err := c.Get(ctx, k); ok || err == nil || errors.Is(err, ErrUnavailable) {
		t.Fatalf("first Get: ok=%v err=%v, want the connection error", ok, err)
	}
	if _, ok, err := c.Get(ctx, k); ok || !errors.Is(err, ErrUnavailable) {
		t.Errorf("second Get: ok=%v err=%v, want ErrUnavailable without dialling", ok, err)
	}
	if err := c.Put(ctx, k, Clip{Audio: []byte("x")}, time.Hour); !errors.Is(err, ErrUnavailable) {
		t.Errorf("Put while rested: err=%v, want ErrUnavailable", err)
	}
}

func TestANilCacheStoresNothing(t *testing.T) {
	var c *Cache
	if err := c.Put(context.Background(), Key{Text: "hi"}, Clip{Audio: []byte("x")}, time.Hour); err != nil {
		t.Errorf("Put: %v", err)
	}
	if _, ok, err := c.Get(context.Background(), Key{Text: "hi"}); ok || err != nil {
		t.Errorf("Get: ok=%v err=%v, want a miss", ok, err)
	}
}

func TestAClipStoredByThePreviousReleaseIsServedWithNoLabel(t *testing.T) {
	got, ok := decode([]byte("v1\naudio/wav\nchirp3-hd\nRIFF\nWAVE"))
	if !ok || string(got.Audio) != "RIFF\nWAVE" || got.ContentType != "audio/wav" || got.Model != "chirp3-hd" || got.Label != "" {
		t.Errorf("decode = %+v ok=%v, want the v1 clip with no label", got, ok)
	}
}

func TestALabelWithALineBreakDoesNotCorruptTheClip(t *testing.T) {
	got, ok := decode(encode(Clip{Audio: []byte("RIFF"), ContentType: "audio/wav", Model: "tts-1", Label: "local\ntts"}))
	if !ok || string(got.Audio) != "RIFF" || got.Label != "local tts" {
		t.Errorf("decode = %+v ok=%v, want the audio intact and the label on one line", got, ok)
	}
}
