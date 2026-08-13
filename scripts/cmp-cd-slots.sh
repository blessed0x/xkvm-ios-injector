#!/bin/bash
# cmp-cd-slots.sh — compare CD special-slot hashes across our signer vs ldid vs
# Apple codesign on the same real iOS binary, parsed with go-macho (validated).
set -euo pipefail
T=$(mktemp -d); trap 'rm -rf "$T"' EXIT
cd /Users/blessed/xKVM

# 1. real iOS binary
xcrun -sdk iphoneos clang -arch arm64 -miphoneos-version-min=13.0 -fobjc-arc \
  -framework UIKit -framework Foundation -x objective-c -o "$T/app" - <<'EOF'
#import <UIKit/UIKit.h>
@interface AD : UIResponder <UIApplicationDelegate> @end
@implementation AD
- (BOOL)application:(UIApplication *)a didFinishLaunchingWithOptions:(NSDictionary *)o { return YES; }
@end
int main(int argc, char *argv[]) { @autoreleasepool { return UIApplicationMain(argc, argv, nil, NSStringFromClass([AD class])); } }
EOF

# 2. sign four ways
cp "$T/app" "$T/apple" && codesign -s - "$T/apple"
cp "$T/app" "$T/ldid"  && ldid -S "$T/ldid"
cp "$T/app" "$T/ldid-resign" && ldid -S -M "$T/ldid-resign"
cp "$T/app" "$T/xkvm"
printf '<?xml version="1.0" encoding="UTF-8"?>\n<!DOCTYPE plist PUBLIC "-//Apple//DTD PLIST 1.0//EN" "http://www.apple.com/DTDs/PropertyList-1.0.dtd">\n<plist version="1.0"><dict><key>get-task-allow</key><true/></dict></plist>\n' > "$T/ents.plist"
mkdir -p ./.verifydriver
printf 'package main\nimport ("fmt"; "os"; "github.com/xkvm/xkvm/internal/macho")\nfunc main() {\n\tb := macho.Bin{Path: os.Args[1]}\n\te, err := os.ReadFile(os.Args[2]); if err != nil { fmt.Println(err); os.Exit(1) }\n\tif err := b.SignWithEntitlements(e, ""); err != nil { fmt.Println("ERR", err); os.Exit(1) }\n}\n' > ./.verifydriver/main.go
go run github.com/xkvm/xkvm/.verifydriver "$T/xkvm" "$T/ents.plist" 2>&1 | tail -2
rm -rf ./.verifydriver

# 3. dump CDs via go-macho's parser
mkdir -p ./.verifydriver
cat > ./.verifydriver/main.go <<'EOF'
package main

import (
	"fmt"
	"os"

	"github.com/blacktop/go-macho"
)

func main() {
	for _, path := range os.Args[1:] {
		f, err := macho.Open(path)
		if err != nil {
			fmt.Printf("%-13s open err: %v\n", path, err)
			continue
		}
		cs := f.CodeSignature()
		if cs == nil || len(cs.CodeDirectories) == 0 {
			fmt.Printf("%-13s no CD\n", path)
			continue
		}
		cd := cs.CodeDirectories[0]
		h := cd.Header
		zero := make([]byte, int(h.HashSize))
		slots := make([]string, 0, int(h.NSpecialSlots))
		for _, s := range cd.SpecialSlots {
			label := ""
			if len(s.Hash) == int(h.HashSize) && string(s.Hash) != string(zero) {
				label = fmt.Sprintf("%x", s.Hash[:4])
			}
			slots = append(slots, fmt.Sprintf("%d=%s", s.Index, label))
		}
		fmt.Printf("%-13s ver=%d flags=%#x nSpec=%d nCode=%d codeLimit=%#x hashType=%d pageBits=%d slots=[%s]\n",
			path, h.Version, uint32(h.Flags), h.NSpecialSlots, h.NCodeSlots, h.CodeLimit, h.HashType, h.PageSize, join(slots, " "))
	}
}

func join(s []string, sep string) string {
	out := ""
	for i, v := range s {
		if i > 0 {
			out += sep
		}
		out += v
	}
	return out
}
EOF
go run github.com/xkvm/xkvm/.verifydriver "$T/apple" "$T/ldid" "$T/ldid-resign" "$T/xkvm"
rm -rf ./.verifydriver

echo "--- codesign --verify results ---"
for f in apple ldid ldid-resign xkvm; do
  printf '%-13s ' "$f"
  out=$(codesign --verify --verbose=2 "$T/$f" 2>&1 | head -1) || true
  echo "$out"
  echo "  exit=${PIPESTATUS[0]}"
done
