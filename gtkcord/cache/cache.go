package cache

import (
	"context"
	"fmt"
	"io"
	"io/ioutil"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"
	"unicode"

	sema "golang.org/x/sync/semaphore"

	"github.com/diamondburned/gtkcord3/gtkcord/gtkutils"
	"github.com/diamondburned/gtkcord3/gtkcord/semaphore"
	"github.com/diamondburned/gtkcord3/internal/log"
	"github.com/gotk3/gotk3/gdk"
	"github.com/gotk3/gotk3/gtk"
	"github.com/pkg/errors"
)

var Client = http.Client{
	Timeout: 15 * time.Second,
}

// DO NOT TOUCH.
const (
	CacheHash   = "hackadoll3"
	CachePrefix = "gtkcord3"
)

var (
	DirName = CachePrefix + "-" + CacheHash
	Temp    = os.TempDir()
	Path    = filepath.Join(Temp, DirName)

	// global HTTP throttler to fetch assets.
	throttler = sema.NewWeighted(int64(runtime.GOMAXPROCS(-1)))
)

// var store *diskv.Diskv

func init() {
	cleanUpCache()
}

func cleanUpCache() {
	tmp, err := os.Open(Temp)
	if err != nil {
		return
	}

	dirs, err := tmp.Readdirnames(-1)
	if err != nil {
		return
	}

	for _, d := range dirs {
		if strings.HasPrefix(d, CachePrefix+"-") && d != DirName {
			path := filepath.Join(Temp, d)
			log.Infoln("Deleting old cache in", path)
			os.RemoveAll(path)
		}
	}
}

func TransformURL(s string) string {
	var sizeSuffix string

	u, err := url.Parse(s)
	if err != nil {
		return filepath.Join(Path, SanitizeString(s)+sizeSuffix)
	}

	path := filepath.Join(Path, u.Hostname())

	if err := os.MkdirAll(path, 0755|os.ModeDir); err != nil {
		log.Errorln("Failed to mkdir:", err)
	}

	return filepath.Join(path, SanitizeString(u.EscapedPath()+"?"+u.RawQuery)+sizeSuffix)
}

// SanitizeString makes the string friendly to put into the file system. It
// converts anything that isn't a digit or letter into underscores.
func SanitizeString(str string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsDigit(r) || r == '#' || r == '.' {
			return r
		}

		return '_'
	}, str)
}

// var fileIO sync.Mutex

func download(ctx context.Context, url string, pp []Processor, gif bool) ([]byte, error) {
	// Throttle.
	if err := throttler.Acquire(ctx, 1); err != nil {
		return nil, errors.Wrap(err, "Failed to acquire throttler")
	}
	defer throttler.Release(1)

	q, err := http.NewRequestWithContext(ctx, "GET", url, nil)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create a new re")
	}

	r, err := Client.Do(q)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to GET")
	}
	defer r.Body.Close()

	if r.StatusCode < 200 || r.StatusCode > 299 {
		return nil, fmt.Errorf("Bad status code %d for %s", r.StatusCode, url)
	}

	var b []byte

	if len(pp) > 0 {
		if gif {
			b, err = ProcessAnimationStream(r.Body, pp)
		} else {
			b, err = ProcessStream(r.Body, pp)
		}
	} else {
		b, err = ioutil.ReadAll(r.Body)
		if err != nil {
			err = errors.Wrap(err, "Failed to download image")
		}
	}

	return b, err
}

// get doesn't check if the file exists
func get(ctx context.Context, url, dst string, pp []Processor, gif bool) error {
	b, err := download(ctx, url, pp, gif)
	if err != nil {
		return err
	}

	if err := ioutil.WriteFile(dst, b, 0644); err != nil {
		return errors.Wrap(err, "Failed to write file to "+dst)
	}

	return nil
}

func GetPixbuf(url string, pp ...Processor) (*gdk.Pixbuf, error) {
	return GetPixbufScaled(url, 0, 0, pp...)
}

func GetPixbufScaled(url string, w, h int, pp ...Processor) (*gdk.Pixbuf, error) {
	// Transform URL:
	dst := TransformURL(url)

	// Try and get the Pixbuf from file:
	p, err := getPixbufFromFile(dst, w, h)
	if err == nil {
		return p, nil
	}

	// Get the image into file (dst)
	if err := get(context.Background(), url, dst, pp, false); err != nil {
		return nil, err
	}

	p, err = getPixbufFromFile(dst, w, h)
	if err != nil {
		return nil, err
	}

	return p, nil
}

func SetImage(url string, img *gtk.Image, pp ...Processor) error {
	return SetImageScaled(url, img, 0, 0, pp...)
}

func SetImageScaled(url string, img *gtk.Image, w, h int, pp ...Processor) error {
	return SetImageScaledContext(context.Background(), url, img, w, h, pp...)
}

func SetImageScaledContext(ctx context.Context,
	url string, img *gtk.Image, w, h int, pp ...Processor) error {

	// Transform URL:
	gif := strings.Contains(url, "gif")

	// I don't like animated gifs
	if gif {
	    url = strings.Replace(url, "gif", "png", -1)
	    gif = false
	}
	dst := TransformURL(url)

	// Try and set the Pixbuf from file:
	if err := setImageFromFile(img, dst, gif, w, h); err == nil {
		return nil
	}

	// Get the image into file (dst)
	if err := get(ctx, url, dst, pp, gif); err != nil {
		return err
	}

	// Try again:
	if err := setImageFromFile(img, dst, gif, w, h); err != nil {
		os.Remove(dst)
		return errors.Wrap(err, "Failed to get pixbuf after downloading")
	}

	return nil
}

// SetImageAsync is not cached.
func SetImageAsync(url string, img *gtk.Image, w, h int) error {
	// Throttle.
	if err := throttler.Acquire(context.Background(), 1); err != nil {
		return errors.Wrap(err, "Failed to acquire throttler")
	}
	defer throttler.Release(1)

	r, err := Client.Get(url)
	if err != nil {
		return errors.Wrap(err, "Failed to GET "+url)
	}
	defer r.Body.Close()

	if r.StatusCode < 200 || r.StatusCode > 299 {
		return fmt.Errorf("Bad status code %d", r.StatusCode)
	}

	var gif = strings.Contains(url, ".gif")

	return setImageStream(r.Body, img, gif, w, h, true)
}

func AsyncFetch(url string, img *gtk.Image, w, h int, pp ...Processor) {
	semaphore.IdleMust(gtkutils.ImageSetIcon, img, "image-missing", w)
	fetchImage(url, img, w, h, pp...)
}

func AsyncFetchUnsafe(url string, img *gtk.Image, w, h int, pp ...Processor) {
	gtkutils.ImageSetIcon(img, "image-missing", w)
	go fetchImage(url, img, w, h, pp...)
}

func fetchImage(url string, img *gtk.Image, w, h int, pp ...Processor) {
	var err error
	if len(pp) == 0 {
		err = SetImageAsync(url, img, w, h)
	} else {
		err = SetImageScaled(url, img, w, h, pp...)
	}
	if err != nil {
		log.Errorln("Failed to get image", url+":", err)
		return
	}
}

func SizeToURL(url string, w, h int) string {
	return url + "?width=" + strconv.Itoa(w) + "&height=" + strconv.Itoa(h)
}

func MaxSize(w, h, maxW, maxH int) (int, int) {
	if w < maxW && h < maxH {
		return w, h
	}

	if w > h {
		h = h * maxW / w
		w = maxW
	} else {
		w = w * maxH / h
		h = maxH
	}

	return w, h
}

func getPixbufFromFile(path string, w, h int) (*gdk.Pixbuf, error) {
	f, err := os.Open(path)
	if err != nil {
		return nil, errors.Wrap(err, "Failed to open file")
	}
	defer f.Close()

	l, err := gdk.PixbufLoaderNew()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to create a pixbuf_loader")
	}

	if w > 0 && h > 0 {
		gtkutils.Connect(l, "size-prepared", func(l *gdk.PixbufLoader, imgW, imgH int) {
			w, h = MaxSize(imgW, imgH, w, h)
			if w != imgW || h != imgH {
				l.SetSize(w, h)
			}
		})
	}

	if _, err := io.Copy(l, f); err != nil {
		return nil, errors.Wrap(err, "Failed to stream to pixbuf_loader")
	}

	if err := l.Close(); err != nil {
		return nil, errors.Wrap(err, "Failed to close pixbuf_loader")
	}

	p, err := l.GetPixbuf()
	if err != nil {
		return nil, errors.Wrap(err, "Failed to get pixbuf")
	}

	return p, nil
}

func setImageFromFile(img *gtk.Image, path string, gif bool, w, h int) error {
	f, err := os.Open(path)
	if err != nil {
		return errors.Wrap(err, "Failed to open file")
	}
	defer f.Close()

	return setImageStream(f, img, gif, w, h, false)
}

func setImageStream(r io.Reader, img *gtk.Image, gif bool, w, h int, stream bool) error {
	l, err := gdk.PixbufLoaderNew()
	if err != nil {
		return errors.Wrap(err, "Failed to create a pixbuf_loader")
	}
	defer l.Close()

	var p interface{}

	var event = "area-updated"
	if !stream {
		// If we're not streaming anything big, calling "closed" would be
		// faster.
		event = "closed"
	}

	semaphore.IdleMust(func() {
		if w > 0 && h > 0 {
			l.Connect("size-prepared", func(l *gdk.PixbufLoader, imgW, imgH int) {
				w, h = MaxSize(imgW, imgH, w, h)

				// If the new sizes don't match, then we need to resize the image:
				if w != imgW || h != imgH {
					l.SetSize(w, h)
				}

				// If the image's size hasn't been set before, we set it:
				if sw, sh := img.GetSizeRequest(); sw < 1 && sh < 1 {
					semaphore.Async(img.SetSizeRequest, w, h)
				}
			})
		}

		l.Connect("area-prepared", func() {
			if gif {
				p, err = l.GetAnimation()
				if err != nil || p == nil {
					log.Errorln("Failed to get animation during area-prepared:", err)
					return
				}
			} else {
				p, err = l.GetPixbuf()
				if err != nil || p == nil {
					log.Errorln("Failed to get pixbuf during area-prepared:", err)
					return
				}
			}
		})

		l.Connect(event, func() {
			if p == nil {
				return
			}

			semaphore.Async(func() {
				if gif {
					img.SetFromAnimation(p.(*gdk.PixbufAnimation))
				} else {
					img.SetFromPixbuf(p.(*gdk.Pixbuf))
				}
			})
		})
	})

	if _, err := io.Copy(l, r); err != nil {
		return errors.Wrap(err, "Failed to stream to pixbuf_loader")
	}

	return nil
}
