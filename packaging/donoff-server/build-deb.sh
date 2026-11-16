#!/bin/sh
# donoff-server_<version>_<arch>.deb: build-deb.sh <out dir> <arch...> (arm64 amd64);
# VERSION= overrides packaging/donoff-server/VERSION (repo.napilab.ru adds ~git.<hash>).
set -e
here=$(cd "$(dirname "$0")" && pwd)
top=$(cd "$here/../.." && pwd)
out=$(mkdir -p "$1" && cd "$1" && pwd)
shift
[ $# -gt 0 ] || set -- arm64
VERSION=${VERSION:-$(cat "$here/VERSION")}
GO=${GO:-go}
for arch in "$@"; do
	r=$out/root-$arch
	rm -rf "$r"
	(cd "$top" && CGO_ENABLED=0 GOOS=linux GOARCH=$arch $GO build -trimpath \
		-ldflags "-s -w -X main.version=$VERSION" -o "$r/usr/bin/donoff-panel" ./cmd/donoff-panel)
	install -D -m 755 "$here/donoff-server" "$r/usr/sbin/donoff-server"
	install -D -m 644 "$here/donoff-panel.service" "$r/usr/lib/systemd/system/donoff-panel.service"
	install -D -m 644 "$here/influxdb-donoff-server.conf" "$r/usr/lib/systemd/system/influxdb.service.d/donoff-server.conf"
	install -D -m 644 "$here/default-donoff-panel" "$r/etc/default/donoff-panel"
	install -D -m 644 "$here/mosquitto-donoff-server.conf" "$r/etc/mosquitto/conf.d/donoff-server.conf"
	install -D -m 644 "$here/avahi-donoff-mqtt.service" "$r/etc/avahi/services/donoff-mqtt.service"
	install -D -m 644 "$here/README.md" "$r/usr/share/doc/donoff-server/README.md"
	install -d "$r/DEBIAN"
	for f in config postinst prerm postrm; do install -m 755 "$here/debian/$f" "$r/DEBIAN/$f"; done
	install -m 644 "$here/debian/templates" "$r/DEBIAN/templates"
	find "$r" -type d -exec chmod 755 {} +; chmod 755 "$r/usr/bin/donoff-panel"
	(cd "$r" && find etc -type f | sed 's|^|/|' | sort) > "$r/DEBIAN/conffiles"
	sed -e "s|@VERSION@|$VERSION|" -e "s|@ARCH@|$arch|" \
		-e "s|@SIZE@|$(du -sk --exclude=DEBIAN "$r" | cut -f1)|" \
		"$here/debian/control.in" > "$r/DEBIAN/control"
	dpkg-deb --root-owner-group --build "$r" "$out/donoff-server_${VERSION}_$arch.deb"
	rm -rf "$r"
done
