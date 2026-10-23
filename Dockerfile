# Single-image build for one-service hosting (e.g. Render's free plan): one
# Python process runs migrations, the API, the scheduler and a worker, and
# serves the built frontend itself. For the multi-service deployment use
# docker-compose.yml, which builds backend/ and frontend/ separately.
# No BuildKit-only syntax, so any Docker builder (including Render's) works.

FROM node:22-alpine AS web
WORKDIR /web
COPY frontend/package.json frontend/package-lock.json ./
RUN npm ci --no-audit --no-fund
COPY frontend/ ./
RUN npm run build

FROM python:3.12-slim AS api
ENV PIP_NO_CACHE_DIR=1 PIP_DISABLE_PIP_VERSION_CHECK=1
COPY backend/requirements.txt /tmp/requirements.txt
RUN python -m venv /venv && /venv/bin/pip install -r /tmp/requirements.txt
COPY backend/taskflow /app/taskflow
RUN /venv/bin/python -m compileall -q /app/taskflow

FROM python:3.12-slim
RUN useradd --system --uid 10001 --no-create-home --shell /usr/sbin/nologin taskflow
COPY --from=api /venv /venv
COPY --from=api /app /app
COPY --from=web /web/dist /web
WORKDIR /app
ENV PATH=/venv/bin:$PATH \
    PYTHONDONTWRITEBYTECODE=1 \
    PYTHONUNBUFFERED=1 \
    STATIC_DIR=/web \
    TASKFLOW_ENV=production \
    TRUST_PROXY_HEADERS=true
USER 10001
EXPOSE 10000
ENTRYPOINT ["python", "-m", "taskflow"]
CMD ["all-in-one"]
