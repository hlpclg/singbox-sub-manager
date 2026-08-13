#!/usr/bin/env perl
use strict;
use warnings;

my $file = 'install-proxy.sh';
open my $in, '<', $file or die $!;
my @lines = <$in>;
close $in;

open my $out, '>', $file or die $!;
my $in_monitor = 0;
for my $line (@lines) {
    if ($line =~ /^# 11\. Monitor/) {
        $in_monitor = 1;
        print $out <<'END_MONITOR';
# 11. Monitor
MONITOR_SVC_TMP="$(mktemp "/etc/systemd/system/.proxyctl-monitor.service.tmp.XXXXXX")"
MONITOR_TIMER_TMP="$(mktemp "/etc/systemd/system/.proxyctl-monitor.timer.tmp.XXXXXX")"

cat > "$MONITOR_SVC_TMP" <<'EOF2'
[Unit]
Description=Proxyctl Health Monitor

[Service]
Type=oneshot
ExecStart=/usr/local/bin/proxyctl monitor
SuccessExitStatus=2
EOF2

cat > "$MONITOR_TIMER_TMP" <<'EOF2'
[Unit]
Description=Proxyctl Health Monitor Timer

[Timer]
OnCalendar=*:0/5
Persistent=true
AccuracySec=15s
RandomizedDelaySec=15s

[Install]
WantedBy=timers.target
EOF2

chmod 0644 "$MONITOR_SVC_TMP" "$MONITOR_TIMER_TMP"
mv "$MONITOR_SVC_TMP" /etc/systemd/system/proxyctl-monitor.service
mv "$MONITOR_TIMER_TMP" /etc/systemd/system/proxyctl-monitor.timer

systemctl daemon-reload
systemctl enable --now proxyctl-monitor.timer

END_MONITOR
        next;
    }
    if ($in_monitor && $line =~ /^# 12\. Sysctl/) {
        $in_monitor = 0;
    }
    if (!$in_monitor) {
        print $out $line;
    }
}
close $out;
