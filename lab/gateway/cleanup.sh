#!/bin/bash
# Removes everything smoke.sh made on the gateway. Safe to run twice. A restart does the same.
B=/tmp/sgtest
PATH=/usr/sbin:/usr/bin:/sbin:/bin
timeout -s KILL 60 $B/runtimelab teardown >/dev/null 2>&1
systemctl stop sidegate-core 2>/dev/null
rm -f /run/systemd/system/sidegate-core.service; systemctl daemon-reload
for u in sgt-web sgt-srvA sgt-srvB; do systemctl stop $u 2>/dev/null; done
iptables -D FORWARD -i sglan0 -j ACCEPT -m comment --comment sgtest 2>/dev/null
iptables -D FORWARD -o sglan0 -j ACCEPT -m comment --comment sgtest 2>/dev/null
for n in 1 2; do iptables -t nat -D POSTROUTING -s 10.71.0.0/24 -o sgclt$n -j MASQUERADE -m comment --comment sgtest 2>/dev/null; done
for l in sgclt1 sgclt2 pxa1 pxa2 sgts0 sglan0; do ip link del "$l" 2>/dev/null; done
ip netns del sgtest 2>/dev/null; ip netns del sgweb 2>/dev/null; ip netns del sglan 2>/dev/null
while ip rule del pref 99 2>/dev/null; do :; done
while ip rule del pref 100 2>/dev/null; do :; done
ip route flush table 100 2>/dev/null
for t in raw mangle filter; do
  iptables-save -t "$t" | sed -n 's/^-A \(PREROUTING\|INPUT\|OUTPUT\) \(.*-j sidegate-.*\)$/\1 \2/p' | while read -r c rest; do eval "iptables -t $t -D $c $rest"; done
  for c in $(iptables-save -t "$t" | sed -n 's/^:\(sidegate-\(prerouting\|input\|output\)\) .*/\1/p'); do iptables -t "$t" -F "$c"; iptables -t "$t" -X "$c"; done
done
ip6tables-save -t filter | sed -n 's/^-A \(INPUT\|OUTPUT\) \(.*-j sidegate-.*\)$/\1 \2/p' | while read -r c rest; do eval "ip6tables -t filter -D $c $rest"; done
for c in $(ip6tables-save -t filter | sed -n 's/^:\(sidegate-\(prerouting\|input\|output\)\) .*/\1/p'); do ip6tables -F "$c"; ip6tables -X "$c"; done
umount /opt/sidegate-lab 2>/dev/null; umount /var/lib/unifi-lab 2>/dev/null
rmdir /opt/sidegate-lab /var/lib/unifi-lab 2>/dev/null
rm -rf /run/sidegate
systemctl stop sgt-deadman.timer 2>/dev/null
exit 0
