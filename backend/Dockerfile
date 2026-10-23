# One image for every backend process: api, scheduler, worker, migrate, seed.
# No BuildKit-only syntax, so any Docker builder works.

FROM python:3.12-slim AS build
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
COPY requirements.txt /tmp/requirements.txt
RUN python -m venv /venv && /venv/bin/pip install -r /tmp/requirements.txt
COPY taskflow /app/taskflow
RUN /venv/bin/python -m compileall -q /app/taskflow

# Runtime: no build tools, unprivileged user, read-only friendly (no .pyc
# writes at runtime; bytecode is compiled above).
FROM python:3.12-slim
RUN useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin taskflow
COPY --from=build /venv /venv
COPY --from=build /app /app
WORKDIR /app
ENV PATH=/venv/bin:$PATH PYTHONDONTWRITEBYTECODE=1 PYTHONUNBUFFERED=1
USER 10001
EXPOSE 8080 9090
ENTRYPOINT ["python", "-m", "taskflow"]
CMD ["api"]
