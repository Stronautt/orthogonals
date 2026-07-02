Name:           orthogonals
# Version comes from the Makefile: make rpm passes --define "pkgver X.Y.Z".
Version:        %{pkgver}
Release:        1%{?dist}
Summary:        Looking Glass Windows 11 VM host setup for Fedora
License:        GPL-3.0-only
URL:            https://github.com/stronautt/orthogonals
Source0:        %{name}-%{version}.tar.gz
# Intel iGPU + NVIDIA dGPU hosts only
ExclusiveArch:  x86_64
BuildRequires:  golang

%description
Same machine, orthogonal axes: Windows at full GPU speed, Linux never pauses.

%prep
%autosetup

%build
CGO_ENABLED=0 go build -ldflags "-X github.com/stronautt/orthogonals/internal/cli.Version=%{version}" -o orthogonals .

%install
install -Dm0755 orthogonals %{buildroot}%{_bindir}/orthogonals

%files
%license LICENSE
%doc README.md
%{_bindir}/orthogonals
