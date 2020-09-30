registry="192.168.29.235:30443/k8s-deploy/dubbo-exporter:v1.0.1"
docker build -t ${registry} .
docker push ${registry}