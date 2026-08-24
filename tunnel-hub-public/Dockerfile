FROM node:25-bookworm AS build

WORKDIR /app
COPY tunnel-hub-public/package*.json ./
RUN npm ci
COPY tunnel-hub-public/ ./
RUN npm run build

FROM nginx:1.27-alpine

COPY tunnel-hub-public/nginx.conf /etc/nginx/conf.d/default.conf
COPY tunnel-hub-public/10-runtime-config.sh /docker-entrypoint.d/10-runtime-config.sh
COPY --from=build /app/dist /usr/share/nginx/html
RUN chmod +x /docker-entrypoint.d/10-runtime-config.sh

EXPOSE 80
