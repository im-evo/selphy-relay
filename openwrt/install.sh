#!/bin/sh
# Installs (or with "uninstall", removes) selphy-relay on OpenWrt / GL.iNet routers.
set -e
cd "$(dirname "$0")"

if [ "$1" = "uninstall" ]; then
	[ -x /etc/init.d/selphy-relay ] && { /etc/init.d/selphy-relay stop; /etc/init.d/selphy-relay disable; }
	rm -f /usr/bin/selphy-relay /etc/init.d/selphy-relay /etc/avahi/services/selphy-relay.service
	sed -i '/selphy-relay/d' /etc/sysupgrade.conf
	killall -HUP avahi-daemon 2>/dev/null || true
	echo "Removed. Config left at /etc/config/selphy-relay (delete it if you like)."
	exit 0
fi

. /etc/openwrt_release
case "$DISTRIB_ARCH" in
	aarch64*) bin=selphy-relay-arm64 ;;
	arm_*) bin=selphy-relay-armv7 ;;
	mipsel*) bin=selphy-relay-mipsle ;;
	mips_*) bin=selphy-relay-mips ;;
	x86_64*) bin=selphy-relay-amd64 ;;
	*) echo "Unsupported architecture: $DISTRIB_ARCH"; exit 1 ;;
esac
[ -f "bin/$bin" ] || { echo "Missing bin/$bin"; exit 1; }
ps w | grep -q "[a]vahi-daemon" || echo "Warning: avahi-daemon is not running; phones won't discover the relay (opkg install avahi-nodbus-daemon)."

[ -x /etc/init.d/selphy-relay ] && /etc/init.d/selphy-relay stop || true
cp "bin/$bin" /usr/bin/selphy-relay && chmod 755 /usr/bin/selphy-relay
cp selphy-relay.init /etc/init.d/selphy-relay && chmod 755 /etc/init.d/selphy-relay
[ -f /etc/config/selphy-relay ] || cp selphy-relay.config /etc/config/selphy-relay
mkdir -p /etc/avahi/services
for f in /usr/bin/selphy-relay /etc/init.d/selphy-relay /etc/config/selphy-relay; do
	grep -qx "$f" /etc/sysupgrade.conf 2>/dev/null || echo "$f" >> /etc/sysupgrade.conf
done
/etc/init.d/selphy-relay enable
/etc/init.d/selphy-relay start
sleep 8
logread -e selphy-relay | tail -5
ip=$(uci -q get network.lan.ipaddr || echo "<router-ip>")
echo
echo "Installed. Status page: http://${ip%%/*}:$(uci -q get selphy-relay.main.listen | sed 's/.*://')/"
