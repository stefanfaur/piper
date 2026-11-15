FROM golang:1.27-alpine AS build
WORKDIR /src
COPY go.mod *.go ./
ARG VERSION=dev
ARG COMMIT=none
ARG BUILT_AT=unknown
RUN CGO_ENABLED=0 go build \
    -ldflags "-X main.version=$VERSION -X main.commit=$COMMIT -X main.builtAt=$BUILT_AT" \
    -o /piper .

FROM scratch
COPY --from=build /piper /piper
ENV DATA_DIR=/data
EXPOSE 8080
ENTRYPOINT ["/piper"]
