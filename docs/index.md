# Force

`Force` is an event processing and infrastructure automation framework.

`Makefiles` create an easy way to build targets and projects.

`G` files create an easy way to create event-driven workflows with multiple services
combined together: Github to Docker builds, Webhooks to Kubernetes Deployments.

## Prerequisites

- Go 1.12+


## Installation

Current version is `0.0.8`.

*Install locally*

```bash
$ go install github.com/gravitational/force/tool/force
```

*Docker image*

```
docker pull gcr.io/kubeadm-167321/force:0.0.8
```

*Local Docker Builds*

To use local force's ability to run builds, install
[runc 1.0.0-rc8](https://github.com/opencontainers/runc/releases/tag/v1.0.0-rc8)