#!/usr/bin/perl

use strict;
use warnings;

use JSON -support_by_pp;

my $file = shift or die "no file argument";
my $trackers = {};

if (not open OLDFILE, $file) { 
    warn "Cannot read data file: $file";
} else {
    my @data = <OLDFILE>;
    my $data = join '', @data;
    $trackers = from_json($data);
}

foreach my $name (keys %{$trackers}) {
    map { print "$name: $_\n" } @{$trackers->{$name}->{'A'}};
    map { print "$name: $_\n" } @{$trackers->{$name}->{'AAAA'}};
}
