target "docker-metadata-action" {}

variable "APP" {
  default = "prober"
}

variable "VERSION" {
  default = "v0.0.1"
}

variable "SOURCE" {
  default = "https://github.com/cilindrox/probe-controller"
}

variable "REGISTRY" {
  default = "ghcr.io/cilindrox/probe-controller"
}

group "default" {
  targets = ["image-local"]
}

target "image" {
  inherits = ["docker-metadata-action"]
  args = {
    VERSION = "${VERSION}"
  }
  labels = {
    "org.opencontainers.image.source" = "${SOURCE}"
  }
}

target "image-local" {
  inherits = ["image"]
  output = ["type=docker"]
  tags = [
    "${APP}:${VERSION}",
  ]
}

target "image-all" {
  inherits = ["image"]
  platforms = [
    "linux/amd64",
    "linux/arm64",
  ]
  tags = [
    "${REGISTRY}/${APP}:${VERSION}",
    "${REGISTRY}/${APP}:latest",
  ]
  cache-from = ["type=registry,ref=${REGISTRY}/${APP}:buildcache"]
  cache-to = ["type=registry,ref=${REGISTRY}/${APP}:buildcache,mode=max"]
}
