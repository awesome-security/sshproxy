SSHPROXY_VERSION ?= 0.4.5
SSHPROXY_GIT_URL ?= github.com/cea-hpc/sshproxy

prefix		?= /usr
bindir		?= $(prefix)/bin
sbindir		?= $(prefix)/sbin
datarootdir	?= $(prefix)/share
mandir		?= $(datarootdir)/man

GO		?= go

ASCIIDOC_OPTS	= -asshproxy_version=$(SSHPROXY_VERSION)
GO_OPTS		= -ldflags "-X main.SshproxyVersion=$(SSHPROXY_VERSION)"

SSHPROXY_SRC		= $(wildcard cmd/sshproxy/*.go)
SSHPROXY_DUMPD_SRC	= $(wildcard cmd/sshproxy-dumpd/*.go)
SSHPROXY_MANAGERD_SRC	= $(wildcard cmd/sshproxy-managerd/*.go)
SSHPROXY_REPLAY_SRC	= $(wildcard cmd/sshproxy-replay/*.go)
MANAGER_SRC		= $(wildcard pkg/manager/*.go)
RECORD_SRC		= $(wildcard pkg/record/*.go)
ROUTE_SRC		= $(wildcard pkg/route/*.go)
UTILS_SRC		= $(wildcard pkg/utils/*.go)

PKGS	= $(shell $(GO) list ./... | grep -v /vendor/)
EXE	= $(addprefix bin/, sshproxy sshproxy-dumpd sshproxy-managerd sshproxy-replay)
MANDOC	= doc/sshproxy.yaml.5 doc/sshproxy-managerd.yaml.5 doc/sshproxy.8 doc/sshproxy-dumpd.8 doc/sshproxy-managerd.8 doc/sshproxy-replay.8

all: exe doc

exe: $(EXE)

doc: $(MANDOC)

%.5: %.txt
	a2x $(ASCIIDOC_OPTS) -f manpage $<

%.8: %.txt
	a2x $(ASCIIDOC_OPTS) -f manpage $<

bin/sshproxy: $(SSHPROXY_SRC) $(MANAGER_SRC) $(RECORD_SRC) $(ROUTE_SRC) $(UTILS_SRC)
	$(GO) build $(GO_OPTS) -o $@ $(SSHPROXY_GIT_URL)/cmd/sshproxy

bin/sshproxy-dumpd: $(SSHPROXY_DUMPD_SRC) $(RECORD_SRC) $(UTILS_SRC)
	$(GO) build $(GO_OPTS) -o $@ $(SSHPROXY_GIT_URL)/cmd/sshproxy-dumpd

bin/sshproxy-managerd: $(SSHPROXY_MANAGERD_SRC) $(ROUTE_SRC) $(UTILS_SRC)
	$(GO) build $(GO_OPTS) -o $@ $(SSHPROXY_GIT_URL)/cmd/sshproxy-managerd

bin/sshproxy-replay: $(SSHPROXY_REPLAY_SRC) $(RECORD_SRC)
	$(GO) build $(GO_OPTS) -o $@ $(SSHPROXY_GIT_URL)/cmd/sshproxy-replay

install: install-binaries install-doc-man

install-doc-man: $(MANDOC)
	install -d $(DESTDIR)$(mandir)/man5
	install -p -m 0644 doc/*.5 $(DESTDIR)$(mandir)/man5
	install -d $(DESTDIR)$(mandir)/man8
	install -p -m 0644 doc/*.8 $(DESTDIR)$(mandir)/man8

install-binaries: $(EXE)
	install -d $(DESTDIR)$(sbindir)
	install -p -m 0755 bin/sshproxy $(DESTDIR)$(sbindir)
	install -p -m 0755 bin/sshproxy-dumpd $(DESTDIR)$(sbindir)
	install -p -m 0755 bin/sshproxy-managerd $(DESTDIR)$(sbindir)
	install -d $(DESTDIR)$(bindir)
	install -p -m 0755 bin/sshproxy-replay $(DESTDIR)$(bindir)

format:
	$(GO) fmt $(PKGS)

lint:
	golint $(PKGS)

vet:
	$(GO) vet $(PKGS)

clean:
	rm -f $(EXE) $(MANDOC) doc/*.xml

.PHONY: all exe doc install install-doc-man install-binaries format lint clean vet
