export DOCKER_BUILDKIT=1
LDFLAG_LOCATION := github.com/metal-automata/conditionorc/internal/version
GIT_COMMIT  := $(shell git rev-parse --short HEAD)
GIT_BRANCH  := $(shell git symbolic-ref -q --short HEAD)
GIT_SUMMARY := $(shell git describe --tags --dirty --always)
VERSION     := $(shell git describe --tags 2> /dev/null)
BUILD_DATE  := $(shell date +%s)
GIT_COMMIT_FULL  := $(shell git rev-parse HEAD)
GO_VERSION := $(shell expr `go version |cut -d ' ' -f3 |cut -d. -f2` \>= 16)
DOCKER_IMAGE  ?= "ghcr.io/metal-automata/conditionorc"
REPO := "https://github.com/metal-automata/conditionorc.git"

.DEFAULT_GOAL := help

## lint
lint:
	golangci-lint run --config .golangci.yml

## lint-fix - auto fix lint errors - for the linters that support auto fix
lint-fix:
	golangci-lint run --config .golangci.yml --fix


## Go test
test: lint
	go test -timeout 1m -v -covermode=atomic ./...

## Generate mocks
gen-mock:
	go install github.com/vektra/mockery/v2@v2.42.1
	mockery
	go mod tidy

## build osx bin
build-osx:
ifeq (${GO_VERSION}, 0)
	$(error build requies go version 1.17.n or higher)
endif
	CGO_ENABLED=0 GOOS=darwin GOARCH=amd64 go build -o conditionorc \
		-ldflags \
		"-X ${LDFLAG_LOCATION}.GitCommit=${GIT_COMMIT} \
		 -X ${LDFLAG_LOCATION}.GitBranch=${GIT_BRANCH} \
		 -X ${LDFLAG_LOCATION}.GitSummary=${GIT_SUMMARY} \
		 -X ${LDFLAG_LOCATION}.AppVersion=${VERSION} \
		 -X ${LDFLAG_LOCATION}.BuildDate=${BUILD_DATE}"

## Build linux bin
build-linux:
ifeq (${GO_VERSION}, 0)
	$(error build requies go version 1.16.n or higher)
endif
	CGO_ENABLED=0 GOOS=linux GOARCH=amd64 go build -o conditionorc \
		-ldflags \
		"-X ${LDFLAG_LOCATION}.GitCommit=${GIT_COMMIT} \
		 -X ${LDFLAG_LOCATION}.GitBranch=${GIT_BRANCH} \
		 -X ${LDFLAG_LOCATION}.GitSummary=${GIT_SUMMARY} \
		 -X ${LDFLAG_LOCATION}.AppVersion=${VERSION} \
		 -X ${LDFLAG_LOCATION}.BuildDate=${BUILD_DATE}"

## build docker image and tag as ghcr.io/metal-automata/conditionorc:latest
build-image: build-linux
	docker build --rm=true -f Dockerfile -t ${DOCKER_IMAGE}:latest . \
		--label org.label-schema.schema-version=1.0 \
		--label org.label-schema.vcs-ref=${GIT_COMMIT_FULL} \
		--label org.label-schema.vcs-url=${REPO}

## build and push devel docker image to KIND image repo
push-image-devel: build-image
	docker tag ${DOCKER_IMAGE}:latest localhost:5001/conditionorc:latest
	docker push localhost:5001/conditionorc:latest
	kind load docker-image localhost:5001/conditionorc:latest

## push docker image
push-image:
	docker push ${DOCKER_IMAGE}:latest

SWAG_INSTALLED := $(shell command -v swag 2> /dev/null)

## generate swagger docs for api
gen-docs:
ifndef SWAG_INSTALLED
	echo "Attempting to install swagger . . ."
	go install github.com/swaggo/swag/cmd/swag@latest
endif
	swag init --parseDependency --parseDepth 1

# experimental
multistage-image:
	docker build -f Dockerfile.builder -t ${DOCKER_IMAGE}:latest . \
		--build-arg GIT_COMMIT=${GIT_COMMIT} \
		--build-arg GIT_BRANCH=${GIT_BRANCH} \
		--build-arg GIT_SUMMARY=${GIT_SUMMARY} \
		--build-arg VERSION=${VERSION} \
		--build-arg BUILD_DATE=${BUILD_DATE} \
		--label org.label-schema.schema-version=1.0 \
		--label org.label-schema.vcs-ref=${GIT_COMMIT_FULL} \
		--label org.label-schema.vcs-url=${REPO}

push-ms-devel: multistage-image
	docker tag ${DOCKER_IMAGE}:latest localhost:5001/conditionorc:latest
	docker push localhost:5001/conditionorc:latest
	kind load docker-image localhost:5001/conditionorc:latest

# https://gist.github.com/prwhite/8168133
# COLORS
GREEN  := $(shell tput -Txterm setaf 2)
YELLOW := $(shell tput -Txterm setaf 3)
WHITE  := $(shell tput -Txterm setaf 7)
RESET  := $(shell tput -Txterm sgr0)

TARGET_MAX_CHAR_NUM=20

## Show help
help:
	@echo ''
	@echo 'Usage:'
	@echo '  ${YELLOW}make${RESET} ${GREEN}<target>${RESET}'
	@echo ''
	@echo 'Targets:'
	@awk '/^[a-zA-Z\-\\_0-9]+:/ { \
		helpMessage = match(lastLine, /^## (.*)/); \
		if (helpMessage) { \
			helpCommand = substr($$1, 0, index($$1, ":")-1); \
			helpMessage = substr(lastLine, RSTART + 3, RLENGTH); \
			printf "  ${YELLOW}%-${TARGET_MAX_CHAR_NUM}s${RESET} ${GREEN}%s${RESET}\n", helpCommand, helpMessage; \
		} \
	} \
	{ lastLine = $$0 }' ${MAKEFILE_LIST}
