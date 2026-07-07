# GoExchange GCP 스트레스 테스트 Terraform

이 폴더는 스트레스 테스트용 GCP 인스턴스 2대(서버, 부하생성)를 만들기 위한 Terraform 구성입니다.

## 생성하는 리소스

- VPC 네트워크 + 서브넷
- 방화벽 규칙 3개 (SSH, 모니터링 포트, API 포트)
- Compute Engine 인스턴스 2대 (server, load-gen)

## 1. gcloud 인증

```powershell
gcloud auth application-default login
gcloud config set project <내 GCP 프로젝트 ID>
```

## 2. SSH 키 만들기

```powershell
ssh-keygen -t ed25519 -C "goexchange-gcp" -f "$env:USERPROFILE\.ssh\goexchange-gcp"
```

## 3. 내 공인 IP 확인

```powershell
Invoke-RestMethod https://checkip.amazonaws.com
```

출력된 IP 뒤에 `/32`를 붙여서 `allowed_admin_cidr`에 넣습니다.

## 4. terraform.tfvars 만들기

```powershell
Copy-Item terraform.tfvars.example terraform.tfvars
```

`terraform.tfvars`를 열고 `gcp_project_id`, `allowed_admin_cidr`, `ssh_public_key_path`를 실제 값으로 수정합니다. 이 파일은 Git에 올리지 않습니다.

## 5. 초기화 및 검증

```powershell
terraform init
terraform fmt
terraform validate
```

## 6. 생성 예정 리소스 확인

```powershell
terraform plan -out tfplan
```

## 7. 실제 생성

```powershell
terraform apply tfplan
```

## 8. 삭제

```powershell
terraform destroy
```

## 주의

- `terraform apply` 이후 Compute Engine, 디스크, 외부 IP 등 GCP 비용이 발생할 수 있습니다.
- `terraform.tfstate`, `terraform.tfvars`, `.terraform` 폴더는 Git에 올리지 않습니다.
- 개인키 파일은 절대 공유하지 않습니다.
