<p align="center">
  <a href="" rel="noopener">
 <img width=200px height=100px src="./ktunnel-logo/cover.png" alt="Ktunnel logo"></a>
</p>

<h3 align="center">ktunnel</h3>
<h4 align="center">A CLI tool that establishes a reverse tunnel between a kubernetes cluster and your local machine</h3>

<div align="center">

  [![Status](https://img.shields.io/badge/status-active-success.svg)]() 
  [![GitHub Issues](https://img.shields.io/github/issues/omrikiei/ktunnel.svg)](https://github.com/omrikiei/ktunnel/issues)
  [![GitHub Pull Requests](https://img.shields.io/github/issues-pr/omrikiei/ktunnel.svg)](https://github.com/omrikiei/ktunnel/pulls)
  [![License: GPL v3](https://img.shields.io/badge/License-GPLv3-blue.svg)](https://www.gnu.org/licenses/gpl-3.0)

</div>

---

<p align="center">Expose your local resources to kubernetes
    <br> 
</p>

## 📝 Table of Contents
- [Installation](#installation)
- [About](#about)
- [Usage](#usage)
- [Documentation](./docs/index.md)
- [Roadmap](./ROADMAP.md)
- [Authors](https://github.com/omrikiei/ktunnel/contributors)

## 🏁 Installation <a name = "installation"></a>
| Distribution                                        | Command / Link                                                                          |
|-----------------------------------------------------|-----------------------------------------------------------------------------------------|
| Pre-built binaries for macOS, Linux, and Windows    | [GitHub releases](https://github.com/omrikiei/ktunnel/releases)                         |
| Homebrew  (macOS and Linux)                         | `brew tap omrikiei/ktunnel && brew install omrikiei/ktunnel/ktunnel`                                     |
| [Krew](https://krew.sigs.k8s.io/)                   | `kubectl krew install tunnel`                                                           |

### Building from source

Clone the project
```
git clone https://github.com/omrikiei/ktunnel; cd ktunnel
```
Build the binary
```
CGO_ENABLED=0 go build -ldflags="-s -w"
```
You can them move it to your bin path
```
sudo mv ./ktunnel /usr/local/bin/ktunnel
```
Test the command
```
ktunnel -h
```

## 🧐 About <a name = "about"></a>
Ktunnel is a CLI tool that establishes a reverse tunnel between a kubernetes cluster and your local machine.
It lets you expose your machine as a service in the cluster or expose it to a specific deployment. 
You can also use the client and server without the orchestration part.
*Although ktunnel is identified with kubernetes, it can also be used as a reverse tunnel on any other remote system*

Ktunnel was born out of the need to access my development host when running applications on kubernetes. 
The aim of this project is to be a holistic solution to this specific problem (accessing the local machine from a kubernetes pod).
If you found this tool to be helpful on other scenarios, or have any suggesstions for new features - I would love to get in touch.

<p align="center">
<img src="./docs/request_sequence.png" alt="Ktunnel schema">
</p>

<p align="center">
<img src="./docs/ktunnel diagram.png" alt="Ktunnel schema">
</p>

## 🎈 Usage <a name="usage"></a>
### Expose your local machine as a service in the cluster
This will allow pods in the cluster to access your local web app (listening on port 8000) via 
http (i.e kubernetes applications can send requests to myapp:80)
```bash
ktunnel expose myapp 80:8000
ktunnel expose myapp 80:8000 -r #deployment & service will be reused if exists or they will be created
```

### Inject to an existing deployment
This will currently only work for deployments with 1 replica - it will expose a listening port on the pod through a tunnel to your local machine
```bash
ktunnel inject deployment mydeployment 3306
```

### Reconnecting
`expose`, `inject` and `client` reconnect on their own when a tunnel drops -- a
dropped VPN, a suspended laptop, a rescheduled pod. Each attempt rebuilds the
whole local side: it re-resolves the tunnel server's pod, rebuilds the
port-forward and reopens the stream, with an exponential backoff from 1s up to
30s that resets once a tunnel has stayed up for a minute.

**Open connections do not survive a reconnect.** The cluster-side listener is
closed and every connection through it is dropped, exactly as any TCP proxy
restart does; a fresh listener binds the same port when the tunnel comes back.

Cluster resources are never recreated by a reconnect. If the deployment is
deleted mid-session, ktunnel keeps retrying and says why, rather than quietly
recreating what you removed.

The default is to retry forever, which is what an interactive session wants.
Under a process supervisor, ask for an exit code instead:

```bash
# exit non-zero the moment the tunnel drops, and let systemd restart it
ktunnel expose myapp 80:8000 --exit-on-disconnect

# or give up after 10 consecutive failed attempts
ktunnel expose myapp 80:8000 --max-reconnect-attempts 10
```

ktunnel exits 0 on Ctrl+C and 1 when it gives up.

### Resource Cleanup
ktunnel now automatically tracks and cleans up resources (deployments and services) when the process exits. This ensures no orphaned resources are left in your cluster, even after unexpected shutdowns.

- Resources are automatically cleaned up when you press Ctrl+C
- A 30-second timeout ensures cleanup doesn't hang indefinitely
- Use the `-v` flag for verbose logging to see cleanup operations
```bash
ktunnel expose myapp 80:8000 -v
```

### Star History

[![Star History Chart](https://api.star-history.com/svg?repos=omrikiei/ktunnel&type=Timeline)](https://star-history.com/#omrikiei/ktunnel&Timeline)

Made with ❤️ in [Gedera](https://en.wikipedia.org/wiki/Gedera)!
