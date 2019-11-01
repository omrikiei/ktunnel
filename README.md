<p align="center">
  <a href="" rel="noopener">
 <img width=200px height=100px src="./ktunnel-logo/cover.png" alt="Ktunnel logo"></a>
</p>

<h3 align="center">ktunnel</h3>

<div align="center">

  [![Status](https://img.shields.io/badge/status-active-success.svg)]() 
  [![GitHub Issues](https://img.shields.io/github/issues/omrikiei/ktunnel.svg)](https://github.com/omrikiei/ktunnel/issues)
  [![GitHub Pull Requests](https://img.shields.io/github/issues-pr/omrikiei/ktunnel.svg)](https://github.com/omrikiei/ktunnel/pulls)
  [![License](https://img.shields.io/badge/license-MIT-blue.svg)](https://opensource.org/licenses/MIT)

</div>

---

<p align="center">Expose your local resources to kubernetes
    <br> 
</p>

## 📝 Table of Contents
- [About](#about)
- [Getting Started](#getting_started)
- [Usage](#usage)
- [Docs](./docs/ktunnel.md)
- [Contributing](../CONTRIBUTING.md)
- [Authors](#authors)
- [Acknowledgments](#acknowledgement)

## 🧐 About <a name = "about"></a>
Ktunnel is a CLI tool that establishes a reverse tunnel between a kubernetes cluster and your local machine. It let's you expose your machine as a service in the cluster or expose it to a specific deployment. You can also use the client and server without the orchestration part.

ktunnel was born out of the need to access my development host when running applications on kubernetes. I specifically found it to be a challenge to run a [remote pycharm debuuger](https://www.jetbrains.com/help/pycharm/remote-debugging-with-product.html#remote-debug-config) on pods in a kubernetes development cluster. 
The aim of this project is to be an holistic solution to this specific problem(accessing the your local machine from a kubernetes pod) - although there are partial solutions to this problem such as [inlets](https://github.com/inlets/inlets) and [ngrok](https://ngrok.com/) I found them to be unsuitable and insecure for the task at hand.
If you found this tool to be helpful on other scenarios(accessing a seeded development database/mocking a service and whatnot) I would love for us to communicate on that.

## 🏁 Getting Started <a name = "getting_started"></a>
These instructions will get you a copy of the project up and running on your local machine for development and testing purposes. See [deployment](#deployment) for notes on how to deploy the project on a live system.

### Prerequisites
You should have proper permissions on the kubernetes cluster

### Installing
## From the releases page
Download [here](https://github.com/omrikiei/ktunnel/releases/) and extract it to a local bin path
## Building from source
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
Test the commamd
```
ktunnel -h
```

## 🎈 Usage <a name="usage"></a>
### Expose your local machine as a headless service in the cluster
This will allow pods in the cluster to access your local web app(listening on port 8000) via http
```bash
ktunnel expose myapp 80:8000
```

### Inject to an existing deployment
This will currently only work for deployments with 1 replica - it will expose a listening port on the pod through a tunnel to your local machine
```bash
ktunnel inject deployment mydeployment 3306
``` 

## ✍️ Authors <a name = "authors"></a>
- [@omrikiei](https://github.com/omrikiei)

See also the list of [contributors](https://github.com/omrikiei/ktunnel/contributors) who participated in this project.