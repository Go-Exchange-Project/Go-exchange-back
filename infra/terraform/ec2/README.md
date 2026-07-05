# GoExchange EC2 Terraform

이 폴더는 GoExchange MVP를 AWS EC2에 올리기 위한 최소 Terraform 구성입니다.

## 생성하는 리소스

- 기본 VPC 조회
- 기본 Subnet 조회
- 최신 Ubuntu 24.04 LTS AMI 조회
- EC2 Key Pair 생성
- Security Group 생성
  - 22번 포트: 내 공인 IP만 허용
  - 80번 포트: 전체 허용
  - 8080번 포트: 전체 허용
- EC2 인스턴스 생성
- Elastic IP 생성 및 EC2 연결

## 1. SSH 키 만들기

EC2에 접속하려면 SSH 키가 필요합니다. PowerShell에서 아래 명령을 실행합니다.

```powershell
ssh-keygen -t ed25519 -C "goexchange-ec2" -f "$env:USERPROFILE\.ssh\goexchange-ec2"
```

생성되는 파일은 두 개입니다.

- `goexchange-ec2`: 개인키입니다. 절대 Git에 올리거나 공유하지 않습니다.
- `goexchange-ec2.pub`: 공개키입니다. Terraform이 AWS Key Pair로 등록합니다.

## 2. 내 공인 IP 확인

PowerShell에서 아래 명령을 실행합니다.

```powershell
Invoke-RestMethod https://checkip.amazonaws.com
```

출력된 IP 뒤에 `/32`를 붙여서 `allowed_ssh_cidr`에 넣습니다.

예:

```hcl
allowed_ssh_cidr = "203.0.113.10/32"
```

## 3. terraform.tfvars 만들기

예시 파일을 복사합니다.

```powershell
Copy-Item terraform.tfvars.example terraform.tfvars
```

`terraform.tfvars`를 열고 아래 값을 수정합니다.

```hcl
allowed_ssh_cidr = "내_공인_IP/32"
public_key_path  = "C:/Users/dksco/.ssh/goexchange-ec2.pub"
```

`terraform.tfvars`는 로컬 설정 파일이므로 Git에 올리지 않습니다.

## 4. Terraform 초기화

```powershell
terraform init
```

이 명령은 AWS provider 플러그인을 내려받고 `.terraform` 폴더를 만듭니다.

## 5. 코드 형식과 문법 확인

```powershell
terraform fmt
terraform validate
```

## 6. 생성 예정 리소스 확인

```powershell
terraform plan -out tfplan
```

여기까지는 실제 리소스를 만들지 않습니다. 어떤 리소스가 생성될지 미리 보여주는 단계입니다.

## 7. 실제 생성

`plan` 결과를 확인한 뒤에만 실행합니다.

```powershell
terraform apply tfplan
```

생성이 끝나면 `public_ip`, `ssh_command`, `frontend_url`, `backend_ping_url`이 출력됩니다.

## 8. 삭제

테스트가 끝나고 비용 발생을 막으려면 아래 명령으로 리소스를 삭제합니다.

```powershell
terraform destroy
```

## 주의

- `terraform apply` 이후 EC2, EBS, Elastic IP 등 AWS 비용이 발생할 수 있습니다.
- `terraform.tfstate`, `terraform.tfvars`, `.terraform` 폴더는 Git에 올리지 않습니다.
- 개인키 파일은 절대 공유하지 않습니다.
