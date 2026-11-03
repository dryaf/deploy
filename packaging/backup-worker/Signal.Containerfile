# The separately qualified base supplies signal-cli, its JVM/native libraries,
# and CA certificates on both target architectures. Never resolve a mutable tag.
ARG SIGNAL_CLI_IMAGE
FROM ${SIGNAL_CLI_IMAGE}
# The pinned upstream JVM image installs its launcher below /opt rather than
# placing it on PATH. Both relay entry points resolve signal-cli by name.
ENV PATH="/opt/signal-cli/bin:/opt/java/openjdk/bin:/usr/local/bin:/usr/bin:/bin"
ARG TARGETARCH
COPY dist/backup-worker/deploy-linux-${TARGETARCH} /usr/local/bin/deploy
USER 0:0
ENTRYPOINT ["/usr/local/bin/deploy"]
