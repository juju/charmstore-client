# Makefile for the charm store client.

ifndef GOPATH
$(warning You need to set up a GOPATH.)
endif

PROJECT := github.com/juju/charmstore-client
PROJECT_DIR := $(shell go list -e -f '{{.Dir}}' $(PROJECT))

INSTALL_FILE=install -m 644 -p
VERSION := $(shell git describe --abbrev=0)

ifeq ($(shell uname -p | sed -r 's/.*(x86|armel|armhf).*/golang/'), golang)
	GO_C := golang
	INSTALL_FLAGS :=
else
	GO_C := gccgo-4.9 gccgo-go
	INSTALL_FLAGS := -gccgoflags=-static-libgo
endif

ifeq ($(VERSION),no)
	VERSIONDEPS :=
else
	VERSIONDEPS := version/init.go
endif

default: build

$(GOPATH)/bin/godeps:
	go get -u -v github.com/rogpeppe/godeps

# Start of GOPATH-dependent targets. Some targets only make sense -
# and will only work - when this tree is found on the GOPATH.

ifeq ($(CURDIR),$(PROJECT_DIR))

build: $(VERSIONDEPS)
	go build $(PROJECT)/...

check: $(VERSIONDEPS)
	go test $(PROJECT)/...

install: $(VERSIONDEPS)
	go install $(INSTALL_FLAGS) -v $(PROJECT)/...

clean:
	go clean $(PROJECT)/...
	rm -rf man

else

build:
	$(error Cannot $@; $(CURDIR) is not on GOPATH)

check:
	$(error Cannot $@; $(CURDIR) is not on GOPATH)

install:
	$(error Cannot $@; $(CURDIR) is not on GOPATH)

clean:
	$(error Cannot $@; $(CURDIR) is not on GOPATH)

endif
# End of GOPATH-dependent targets.

# Reformat source files.
format:
	gofmt -w -l .

# Reformat and simplify source files.
simplify:
	gofmt -w -l -s .

# Update the project Go dependencies to the required revision.
deps: $(GOPATH)/bin/godeps
	$(GOPATH)/bin/godeps -u dependencies.tsv

# Generate the dependencies file.
create-deps: $(GOPATH)/bin/godeps
	godeps -t $(shell go list $(PROJECT)/...) > dependencies.tsv || true

# Generate version information
version/init.go: version/init.go.template FORCE
	gofmt -r "unknownVersion -> \"${VERSION}\"" $< > $@

# Generate man pages.
man/man1:
	make install
	mkdir -p man/man1
	cd man/man1 && ../../scripts/generate-all-manpages.sh

# The install-man make target are for use by debian packaging.
# The semantics should match autotools make files as dh_make expects it.
install-man: man/man1
	mkdir -p $(DESTDIR)/usr/share/man/man1
	for file in man/man1/* ; do \
	 	$(INSTALL_FILE) $$file "$(DESTDIR)/usr/share/man/man1" ; done

uninstall-man: man/man1
	for file in man/man1/* ; do \
	 	rm "$(DESTDIR)/usr/share/$$file" ; done
	-rmdir -p $(DESTDIR)/usr/share/man/man1

install-bash-completion:
	mkdir -p $(DESTDIR)/usr/share/bash-completion/completions
	$(INSTALL_FILE) config/bash/charm "$(DESTDIR)/usr/share/bash-completion/completions/"

uninstall-bash-completion:
	rm "$(DESTDIR)/usr/share/bash-completion/completions/charm"
	-rmdir -p $(DESTDIR)/usr/share/bash-completion/completions

help:
	@echo -e 'Charmstore-client - list of make targets:\n'
	@echo 'make - Build the package.'
	@echo 'make check - Run tests.'
	@echo 'make install - Install the package to $$GOPATH/bin'
	@echo 'make clean - Remove object files from package source directories.'
	@echo 'make deps - Set up the project Go dependencies.'
	@echo 'make create-deps - Generate the Go dependencies file.'
	@echo 'make format - Format the source files.'
	@echo 'make man - Generate man pages.'
	@echo 'make simplify - Format and simplify the source files.'
	@echo 'make install-man - Install man pages to $$DESTDIR/usr/share/man'
	@echo 'make uninstall-man - Remove man pages from $$DESTDIR/usr/share/man'
	@echo 'make install-bash-completion - Install completion to $$DESTDIR/usr/share/bash-completion/completions/'
	@echo 'make uninstall-bash-completion - Remove completion from $$DESTDIR/usr/share/bash-completion/completions/'

.PHONY: build check clean create-deps deps FORCE format help install simplify \
	install-man uninstall-man install-bash-completion uninstall-bash-completion
