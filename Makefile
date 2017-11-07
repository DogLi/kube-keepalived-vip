all: push

# 0.0 shouldn't clobber any release builds
TAG = 0.23
PREFIX = 10.10.20.3:5000/ylf/kube-keepalived-vip
BUILD_IMAGE = build-keepalived
PKG = github.com/aledbf/kube-keepalived-vip

GO_LIST_FILES=$(shell go list ${PKG}/... | grep -v vendor)

controller: clean
	CGO_ENABLED=0 GOOS=linux go build -a -installsuffix cgo -ldflags '-s -w' \
	-o rootfs/kube-keepalived-vip \
	${PKG}

container:
	docker build -t $(PREFIX):$(TAG) rootfs

push: container
	docker push $(PREFIX):$(TAG)

clean:
	rm -f rootfs/kube-keepalived-vip

.PHONY: fmt
fmt:
	@go list -f '{{if len .TestGoFiles}}"gofmt -s -l {{.Dir}}"{{end}}' ${GO_LIST_FILES} | xargs -L 1 sh -c

.PHONY: lint
lint:
	@go list -f '{{if len .TestGoFiles}}"golint -min_confidence=0.85 {{.Dir}}/..."{{end}}' ${GO_LIST_FILES} | xargs -L 1 sh -c

.PHONY: test
test:
	@go test -v -race -tags "$(BUILDTAGS) cgo" ${GO_LIST_FILES}

.PHONY: cover
cover:
	@go list -f '{{if len .TestGoFiles}}"go test -coverprofile={{.Dir}}/.coverprofile {{.ImportPath}}"{{end}}' ${GO_LIST_FILES} | xargs -L 1 sh -c
	gover
	goveralls -coverprofile=gover.coverprofile -service travis-ci

.PHONY: vet
vet:
	@go vet ${GO_LIST_FILES}
