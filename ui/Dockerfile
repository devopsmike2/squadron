# Simple development container with hot-reloading
FROM node:20-alpine

WORKDIR /app

# Install pnpm globally for faster package management. Pinned to pnpm 9
# to match the lockfile (lockfileVersion 9.0) and keep image builds
# reproducible; unpinned pulls pnpm 10, whose build-script gating breaks
# the frozen install. See knowledge/ci-gotchas.md.
RUN npm install -g pnpm@9.15.9

# Copy package files first (for better caching)
COPY package*.json ./

# Install dependencies
RUN pnpm install

# Expose Vite dev server port
EXPOSE 5173

# Start development server with hot-reload
CMD ["pnpm", "dev", "--host", "0.0.0.0"]
