# Build with `make docker`, which passes the git-derived identity as build args. The image never
# runs git: .git is not in the build context, and the defaults mark an image built without them.
FROM golang:1.26 AS build
WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY internal ./internal
ARG VERSION=dev
ARG COMMIT=none
ARG BUILT=unknown
RUN CGO_ENABLED=0 go build -trimpath \
      -ldflags "-s -w -X github.com/veritas501/mem-grep/internal/version.Version=${VERSION} \
                -X github.com/veritas501/mem-grep/internal/version.Commit=${COMMIT} \
                -X github.com/veritas501/mem-grep/internal/version.Built=${BUILT}" \
      -o /out/ ./cmd/mg

FROM gcr.io/distroless/static-debian12:nonroot
COPY --from=build /out/mg /usr/local/bin/
# Mount the session directories read-only and point mg at them with --path.
ENTRYPOINT ["/usr/local/bin/mg"]
CMD ["version"]
