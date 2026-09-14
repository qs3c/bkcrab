FROM alpine:3.21
RUN apk add --no-cache python3 quota e2fsprogs-extra docker-cli
COPY quota.py /quota.py
ENTRYPOINT ["python3", "/quota.py"]
