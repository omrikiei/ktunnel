<p align="center">
  <a href="" rel="noopener">
 <img width=400px height=200px src="./ktunnel-logo/cover.png" alt="Ktunnel logo"></a>
</p>

<h3 align="center">Ktunnel</h3>

<div align="center">

  [![Status](https://img.shields.io/badge/status-active-success.svg)]() 
  [![GitHub Issues](https://img.shields.io/github/issues/omrikiei/ktunnel.svg)](https://github.com/omrikiei/ktunnel/issues)
  [![GitHub Pull Requests](https://img.shields.io/github/issues-pr/omrikiei/ktunnel.svg)](https://github.com/omrikiei/ktunnel/pulls)
  [![License](https://img.shields.io/badge/license-MIT-blue.svg)](/LICENSE)

</div>

---

<p align="center"> A reverse tunnel from kubernetes to your local machine.
    <br> 
</p>

## 📝 Table of Contents
- [About](#about)
- [Getting Started](#getting_started)
- [Usage](#usage)
- [Built Using](#built_using)
- [TODO](../TODO.md)
- [Contributing](../CONTRIBUTING.md)
- [Authors](#authors)
- [Acknowledgments](#acknowledgement)

## 🧐 About <a name = "about"></a>
Ktunnel was born out of the need to access my development host when running applications on kubernetes. I specifically found it to be a challenge to run a [remote pycharm debuuger](https://www.jetbrains.com/help/pycharm/remote-debugging-with-product.html#remote-debug-config) on pods in a kubernetes development cluster. 
Although there aren't many use cases that would justify establishing this kind of connection, I have found the tunnel to be super helpful. The aim of this project is to be an holistic solution to this specific problem(accessing the your local machine from a kubernetes pod) - although there are partial solutions to this problem such as [inlets](https://github.com/inlets/inlets) and [ngrok](https://ngrok.com/) I found them to be unsuitable and insecure for the task at hand.
If you found this tool to be helpful on other scenarios(accessing a seeded development database/mocking a service and whatnot) I would love for us to communicate on that.

## 🏁 Getting Started <a name = "getting_started"></a>
These instructions will get you a copy of the project up and running on your local machine for development and testing purposes. See [deployment](#deployment) for notes on how to deploy the project on a live system.

### Prerequisites
You should have proper permissions on the kubernetes cluster

### Installing
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

## 🔧 Running the tests <a name = "tests"></a>
### TODO

### Break down into end to end tests
#### TODO Explain what these tests test and why

### And coding style tests
#### TODO Explain what these tests test and why


## 🎈 Usage <a name="usage"></a>
TODO

## ⛏️ Built Using <a name = "built_using"></a>
- [MongoDB](https://www.mongodb.com/) - Database
- [Express](https://expressjs.com/) - Server Framework
- [VueJs](https://vuejs.org/) - Web Framework
- [NodeJs](https://nodejs.org/en/) - Server Environment

## ✍️ Authors <a name = "authors"></a>
- [@omrikiei](https://github.com/omrikiei) - Idea & Initial work

See also the list of [contributors](https://github.com/omrikiei/ktunnel/contributors) who participated in this project.

## 🎉 Acknowledgements <a name = "acknowledgement"></a>
- TODO