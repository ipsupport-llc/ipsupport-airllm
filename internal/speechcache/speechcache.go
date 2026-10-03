// Package speechcache keeps synthesized clips so that a phrase an alias
// speaks again is answered without calling the provider.
//
// A clip is stored under everything that decides what it sounds like: the
// alias, the provider that actually spoke, the voice and model it spoke with
// after the target's voice mapping, the language, the output format and the
// text. Two tiers never share a clip, so a backup voice's clips are never
// served for the primary, and the primary's clips are still there when it
// recovers. Editing a voice mapping changes the key of the voices it
// touches, so their old clips simply stop matching and age out.
//
// Clips live in Redis, so every gateway replica shares them. The cache is
// best-effort: a Redis error is a miss, and a clip that could not be stored
// is synthesized again next time.
package speechcache

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"sync"
	"time"

	"github.com/redis/go-redis/v9"
)

// Key names one clip.
type Key struct {
	Alias, Provider, Model, Voice, Language, Format, Text string
}

// Clip is a cached synthesis: the audio, its content type, and the model the
// provider reported as having spoken it, which the ledger prices.
type Clip struct {
	Audio       []byte
	ContentType string
	Model       string
}

// MaxClipBytes is the largest clip kept; a longer one is served but not
// stored, so one long text cannot take a large share of Redis.
const MaxClipBytes = 4 << 20

// Cache stores clips. The zero of *Cache (nil) stores nothing.
type Cache struct {
	now       func() time.Time
	rdb       *redis.Client // nil keeps clips in this process
	opTimeout time.Duration

	mu    sync.Mutex
	local map[string]localClip
	// remoteDownTill is when to try Redis again after it failed. It runs on
	// the wall clock, not now: it is about the network, not clip expiry.
	remoteDownTill time.Time
}

// ErrUnavailable is what Get and Put return while Redis is being given a
// rest after an error.
var ErrUnavailable = errors.New("synthesis cache unavailable")

// remoteRetryAfter is how long the cache stays away from Redis after an
// error, so an outage costs a speech request one timeout per interval
// rather than one per request.
const remoteRetryAfter = 5 * time.Second

type localClip struct {
	clip  Clip
	until time.Time
}

// Option configures a Cache.
type Option func(*Cache)

// WithClock replaces time.Now for in-process expiry, for tests.
func WithClock(now func() time.Time) Option { return func(c *Cache) { c.now = now } }

// New returns a cache backed by rdb; nil rdb keeps clips in this process,
// unbounded, which is meant for tests.
func New(rdb *redis.Client, opts ...Option) *Cache {
	c := &Cache{now: time.Now, rdb: rdb, opTimeout: 250 * time.Millisecond, local: map[string]localClip{}}
	for _, o := range opts {
		o(c)
	}
	return c
}

// Get returns the clip stored under k. ok is false on a miss; err is set
// when the store could not be read, which the caller treats as a miss.
func (c *Cache) Get(ctx context.Context, k Key) (clip Clip, ok bool, err error) {
	if c == nil {
		return Clip{}, false, nil
	}
	id := k.id()
	if c.rdb == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		lc, found := c.local[id]
		if !found || !c.now().Before(lc.until) {
			delete(c.local, id)
			return Clip{}, false, nil
		}
		return lc.clip, true, nil
	}
	if !c.remoteUp() {
		return Clip{}, false, ErrUnavailable
	}
	cctx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()
	raw, err := c.rdb.Get(cctx, id).Bytes()
	if errors.Is(err, redis.Nil) {
		return Clip{}, false, nil
	}
	if err != nil {
		c.remoteFailed(ctx)
		return Clip{}, false, err
	}
	clip, ok = decode(raw)
	return clip, ok, nil
}

// Put stores clip under k for ttl. A clip over MaxClipBytes, or a ttl that
// is not positive, is not stored.
func (c *Cache) Put(ctx context.Context, k Key, clip Clip, ttl time.Duration) error {
	if c == nil || ttl <= 0 || len(clip.Audio) > MaxClipBytes {
		return nil
	}
	id := k.id()
	if c.rdb == nil {
		c.mu.Lock()
		defer c.mu.Unlock()
		c.local[id] = localClip{clip: clip, until: c.now().Add(ttl)}
		return nil
	}
	if !c.remoteUp() {
		return ErrUnavailable
	}
	cctx, cancel := context.WithTimeout(ctx, c.opTimeout)
	defer cancel()
	if err := c.rdb.Set(cctx, id, encode(clip), ttl).Err(); err != nil {
		c.remoteFailed(ctx)
		return err
	}
	return nil
}

func (c *Cache) remoteUp() bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	return !time.Now().Before(c.remoteDownTill)
}

// remoteFailed rests Redis after an error, unless the error was the
// caller's own context ending, which says nothing about Redis.
func (c *Cache) remoteFailed(ctx context.Context) {
	if ctx.Err() != nil {
		return
	}
	c.mu.Lock()
	c.remoteDownTill = time.Now().Add(remoteRetryAfter)
	c.mu.Unlock()
}

// id is the Redis key of k: the alias in clear, so an operator can find or
// drop one alias's clips, and a hash of the rest, so the key stays short
// whatever the text. Every field is length-prefixed before hashing, so no
// two keys share an id by moving a separator.
func (k Key) id() string {
	h := sha256.New()
	for _, f := range []string{k.Provider, k.Model, k.Voice, k.Language, k.Format, k.Text} {
		_ = binary.Write(h, binary.BigEndian, uint32(len(f)))
		h.Write([]byte(f))
	}
	return "air:tts:" + k.Alias + ":" + hex.EncodeToString(h.Sum(nil))
}

// The stored value is a version line, the content type and the model on a
// line each, then the audio.
const valueVersion = "v1"

func encode(c Clip) []byte {
	var b bytes.Buffer
	b.Grow(len(valueVersion) + len(c.ContentType) + len(c.Model) + 3 + len(c.Audio))
	b.WriteString(valueVersion + "\n" + c.ContentType + "\n" + c.Model + "\n")
	b.Write(c.Audio)
	return b.Bytes()
}

// decode reads a stored value; anything it does not recognise is a miss.
func decode(raw []byte) (Clip, bool) {
	parts := bytes.SplitN(raw, []byte("\n"), 4)
	if len(parts) != 4 || string(parts[0]) != valueVersion {
		return Clip{}, false
	}
	return Clip{ContentType: string(parts[1]), Model: string(parts[2]), Audio: parts[3]}, true
}
