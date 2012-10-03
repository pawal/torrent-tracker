#!/usr/bin/perl

use strict;
use warnings;

use URI;
use Net::DNS;
use List::Compare;
use JSON -support_by_pp;
use Fcntl qw(:flock);
use Data::Dumper;

# options
my $file = 'data.json';
my $oldfile = $file.'.old';
my $trackerlist = 'list.txt';
my $resolver = '127.0.0.1';
my $DEBUG = 0;

sub getHosts {
    my $file = shift;
    open TRACKERS, $file or die "Cannot read trackers list from file $file";
    my @trackers = <TRACKERS>;
    close TRACKERS;

    my @hosts;
    foreach (@trackers) {
	s/^udp(.*)/http$1/; # udp is not a recognized url
	my $url = URI->new($_);
	my $domain = $url->host;
	push @hosts, $domain;
    }
    # return a unique array
    my %unique;
    for (@hosts) { $unique{$_}++; }
    @hosts = sort keys %unique;
    return \@hosts;
}

sub collectDNS {
    my $hosts = shift;

    my $res = Net::DNS::Resolver->new;
    $res->nameservers($resolver);
    $res->recurse(1);

    my $answer;
    my $result;

    # resolve names
    foreach my $name (@$hosts) {
	# initialize arrays for completeness in later comparisons
	$result->{$name}->{'A'}    = [];
	$result->{$name}->{'AAAA'} = [];

	# query for A record
        $answer = $res->send($name,'A');
	if (defined $answer) {
	    foreach my $data ($answer->answer)
	    {
		if ($data->type eq 'A') {
		    print "$name: ".$data->address." (".$data->ttl.")\n" if $DEBUG;
		    push @{$result->{$name}->{'A'}}, $data->address;
		}
	    }
	}

	# query for AAAA record
        $answer = $res->send($name,'AAAA');
	if (defined $answer) {
	    foreach my $data ($answer->answer)
	    {
		if ($data->type eq 'AAAA') {
		    print "$name: ".$data->address." (".$data->ttl.")\n" if $DEBUG;
		    push @{$result->{$name}->{'AAAA'}}, $data->address;
		}
	    }
	}
    }
    return $result;
}

# compare the A and AAAA records for a host and return diff
sub compareHost {
    my $old = shift;
    my $new = shift;

    my @removed;
    my @added;
    # A
    {
	my $lc = List::Compare->new($old->{'A'},$new->{'A'});
	push @removed, $lc->get_unique;
	push @added, $lc->get_complement;
    }

    # AAAA
    {
	my $lc = List::Compare->new($old->{'AAAA'},$new->{'AAAA'});
	push @removed, $lc->get_unique;
	push @added, $lc->get_complement;
    }
    return \@removed, \@added;
}

# compare the whole set differences for the array of hosts
sub findDifferences {
    my $old = shift;
    my $new = shift;
    my @diff;
    foreach my $name ( keys %{$new} ) {
	# old name might not exist
	if (not defined $old->{$name}) {

	    $old->{$name}->{'A'}    = [];
	    $old->{$name}->{'AAAA'} = [];
	}
	my ($removed, $added) = compareHost($old->{$name}, $new->{$name});
	# TODO: before adding, maybe do a reverse lookup on the IP?
	map { push @diff, "- $name $_" } @$removed;
	map { push @diff, "+ $name $_" } @$added;
    }
    my $result;
    map { $result .= "$_\n" } @diff;
    return $result;
}

sub getOldFile {
    my $file = shift;
    my $oldresult = {};
    if (not open OLDFILE, $file) { 
	warn "Cannot read old file: $file";
    } else {
	my @olddata = <OLDFILE>;
	my $olddata = join '', @olddata;
	$oldresult = from_json($olddata);
    }
    close OLDFILE;
    return $oldresult;
}

sub sendReport {
    my $diff = shift;

    my $subject = 'Tracker report';
    my $to      = 'pawal@iis.se';
    my $from    = 'pawal@snake.blipp.com';

    open(MAIL, "|/usr/sbin/sendmail -t") or die "Cannot send e-mail!";
    print MAIL "To: $to\n";
    print MAIL "From: $from\n";
    print MAIL "Subject: $subject\n\n";
    print MAIL $diff;
    close(MAIL);
}

sub main {
# this should only run one process at a time
    unless (flock(DATA, LOCK_EX|LOCK_NB)) {
	print "$0 is already running. Exiting.\n";
	exit(1);
    }
    my $oldresult = getOldFile($file);
    my $hosts = getHosts($trackerlist);
    my $result = collectDNS($hosts);
    rename $file, $oldfile;
    open(OUT, '>', $file) or die $!;
    print OUT to_json($result, { utf8 => 1 });
    close(OUT);
    my $diff = findDifferences($oldresult,$result);
    if (defined $diff and length($diff) > 0) {
	sendReport($diff);
    }
}

main();

__DATA__
Data used for locking.
