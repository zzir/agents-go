FROM debian:13-slim

ENV DEBIAN_FRONTEND=noninteractive \
    LANG=C.UTF-8 \
    LC_ALL=C.UTF-8 \
    TZ=UTC

RUN apt-get update && \
    apt-get install -y --no-install-recommends \
        bash \
        bash-completion \
        sudo \
        build-essential \
        pkg-config \
        cmake \
        ninja-build \
        git \
        git-lfs \
        curl \
        wget \
        ca-certificates \
        unzip \
        zip \
        jq \
        ripgrep \
        fd-find \
        file \
        procps \
        openssh-client \
        python3 \
        python3-pip \
        python3-venv \
        python-is-python3 \
    && rm -rf /var/lib/apt/lists/*

RUN ln -sf /usr/bin/fdfind /usr/local/bin/fd

RUN useradd -m -u 1000 -s /bin/bash sandbox && \
    echo "sandbox ALL=(ALL) NOPASSWD:ALL" >/etc/sudoers.d/sandbox && \
    mkdir -p /workspace && \
    chown -R sandbox:sandbox /workspace

ENV MISE_DATA_DIR=/opt/mise \
    MISE_INSTALL_PATH=/usr/local/bin/mise \
    PATH="/opt/mise/shims:/usr/local/bin:${PATH}" \
    UV_INSTALL_DIR=/usr/local/bin

RUN curl -fsSL https://mise.run | sh && mise --version && \
    mkdir -p /opt/mise && chown -R sandbox:sandbox /opt/mise

RUN curl -LsSf https://astral.sh/uv/install.sh | sh && uv --version

USER sandbox

ENV HOME=/home/sandbox

WORKDIR /workspace

CMD ["/bin/bash"]