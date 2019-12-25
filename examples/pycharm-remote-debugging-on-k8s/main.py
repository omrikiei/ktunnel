from flask import Flask
import random
import logging
import sys
sys.path.append("pydevd-pycharm.egg")
import pydevd_pycharm

# ==============this code added==================================================================:
import os

# ================================================================================================

app = Flask(__name__)


@app.route('/debug')
def debug():
    if os.environ.get("REMOTE_DEBUG_HOST"):
        host, port = os.environ.get("REMOTE_DEBUG_HOST").split(":")
        logging.info(f"Connecting to remote debugger on {host}:{port}")
        pydevd_pycharm.settrace(host, port=int(port), stdoutToServer=True, stderrToServer=True, suspend=False)
    return "connected!"


@app.route('/')
def hello_ktunnel():
    r = random.randint(0,1000)
    return f'Hello, Ktunnel! random number: {r}'


if __name__ == '__main__':
    logging.getLogger().setLevel(logging.DEBUG)
    random.seed()
    app.run(host="0.0.0.0", port=int(os.environ.get("PORT", 8002)), debug=True)
