FROM node:25-bookworm AS build

WORKDIR /app
COPY tunnel-hub-public/package*.json ./
RUN npm ci
COPY tunnel-hub-public/ ./
ARG BRAND_CONFIG_FILE=configs/brand.yaml
COPY ${BRAND_CONFIG_FILE} /brand/brand.yaml
ENV BRAND_CONFIG_FILE=/brand/brand.yaml
RUN npm run build

FROM nginx:1.27-alpine

COPY tunnel-hub-public/nginx.conf /etc/nginx/conf.d/default.conf
COPY --from=build /app/dist /usr/share/nginx/html

EXPOSE 80
