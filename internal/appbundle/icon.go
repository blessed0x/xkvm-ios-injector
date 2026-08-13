package appbundle

import (
	"fmt"
	"image"
	_ "image/jpeg" // support .jpg/.jpeg icons like cyan (PIL)
	"image/png"
	"os"
	"path/filepath"

	"golang.org/x/image/draw"

	"github.com/xkvm/xkvm/internal/log"
	"github.com/xkvm/xkvm/internal/plist"
)

// ChangeIcon replaces the app icon: the source is scaled to 120x120 and
// 152x152 PNGs placed in the bundle root with a unique cyan-style prefix, and
// CFBundleIcons / CFBundleIcons~ipad point at them. Port of cyan's
// AppBundle.change_icon, minus the pillow dependency.
func (b *Bundle) ChangeIcon(src string) error {
	f, err := os.Open(src)
	if err != nil {
		return err
	}
	defer f.Close()
	img, _, err := image.Decode(f)
	if err != nil {
		return fmt.Errorf("couldn't decode icon %s: %w", src, err)
	}

	uid := randomSuffix()
	i60 := uid + "60x60" // 120x120 @2x
	i76 := uid + "76x76" // 152x152 @2x~ipad
	f60 := i60 + "@2x.png"
	f76 := i76 + "@2x~ipad.png"
	if err := writeScaledPNG(filepath.Join(b.Path, f60), img, 120); err != nil {
		return err
	}
	if err := writeScaledPNG(filepath.Join(b.Path, f76), img, 152); err != nil {
		return err
	}

	icons := dictAt(b.Info, "CFBundleIcons")
	iconsIPad := dictAt(b.Info, "CFBundleIcons~ipad")
	icons["CFBundlePrimaryIcon"] = map[string]any{
		"CFBundleIconFiles": []any{i60},
		"CFBundleIconName":  uid,
	}
	iconsIPad["CFBundlePrimaryIcon"] = map[string]any{
		"CFBundleIconFiles": []any{i60, i76},
		"CFBundleIconName":  uid,
	}
	b.Info["CFBundleIcons"] = icons
	b.Info["CFBundleIcons~ipad"] = iconsIPad

	if err := b.Save(); err != nil {
		return err
	}
	log.Infof("updated app icon")
	return nil
}

func writeScaledPNG(path string, src image.Image, size int) error {
	dst := image.NewRGBA(image.Rect(0, 0, size, size))
	draw.CatmullRom.Scale(dst, dst.Bounds(), src, src.Bounds(), draw.Over, nil)
	out, err := os.Create(path)
	if err != nil {
		return err
	}
	defer out.Close()
	return png.Encode(out, dst)
}

// dictAt returns the Dict at key, creating it if missing (cyan's `if key not
// in plist: plist[key] = {}` merge pattern).
func dictAt(d plist.Dict, key string) plist.Dict {
	existing, ok := d[key].(plist.Dict)
	if !ok {
		existing = plist.Dict{}
		d[key] = existing
	}
	return existing
}
