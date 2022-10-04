FROM golang:1.19 as build
WORKDIR /code
COPY go.* ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 go build

FROM alpine:latest
RUN apk --no-cache add ca-certificates
COPY --from=build /code/forumcleaner ./
CMD ["./forumcleaner"]
