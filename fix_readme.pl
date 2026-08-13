#!/usr/bin/env perl
use strict;
use warnings;

my $file = 'README.md';
open my $in, '<', $file or die $!;
my @lines = <$in>;
close $in;

open my $out, '>', $file or die $!;
for my $line (@lines) {
    if ($line =~ /v0\.4/) {
        $line =~ s/v0\.4/v0\.6/g;
    }
    if ($line =~ /节点健康检查/) {
        $line =~ s/节点健康检查/节点健康检查 (已实现)/;
    }
    if ($line =~ /自动故障切换/) {
        $line =~ s/自动故障切换/自动故障切换与恢复 (已实现)/;
    }
    print $out $line;
}
close $out;
