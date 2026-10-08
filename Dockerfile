FROM node:22-alpine AS frontend-build

WORKDIR /ui
COPY cmd/controlplane/ui/package.json cmd/controlplane/ui/package-lock.json ./
RUN npm ci
COPY cmd/controlplane/ui/ ./
RUN npm run build

FROM golang:1.25-alpine AS build

WORKDIR /src
COPY go.mod go.sum ./
RUN go mod download
COPY cmd ./cmd
COPY --from=frontend-build /ui/dist ./cmd/controlplane/ui/dist
RUN CGO_ENABLED=0 GOOS=linux go build -trimpath -ldflags="-s -w" \
    -o /out/opamp-control-plane ./cmd/controlplane

FROM gcr.io/distroless/static-debian12:nonroot

COPY --from=build /out/opamp-control-plane /opamp-control-plane

EXPOSE 4320 4321
ENTRYPOINT ["/opamp-control-plane"]
