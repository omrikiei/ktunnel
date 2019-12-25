FROM python:3.7-alpine
WORKDIR /run
COPY requirements.txt /tmp/
RUN pip install -r /tmp/requirements.txt
#COPY pydevd-pycharm.egg /run/pydevd-pycharm.egg
COPY main.py /run/main.py

CMD [ "python", "/run/main.py" ]
