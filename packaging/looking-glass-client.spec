Name:           looking-glass-client
# version comes from the Makefile (artifacts.LookingGlassVersion)
Version:        %{lgver}
Release:        3%{?dist}
Summary:        Looking Glass client — low-latency KVMFR frame-relay viewer

License:        GPL-2.0-or-later
URL:            https://looking-glass.io
Source0:        %{name}-%{version}.tar.gz
ExclusiveArch:  x86_64

# upstream's Fedora build-dependency list
BuildRequires:  cmake
BuildRequires:  make
BuildRequires:  gcc
BuildRequires:  gcc-c++
BuildRequires:  pkgconf-pkg-config
BuildRequires:  binutils-devel
BuildRequires:  libglvnd-devel
BuildRequires:  fontconfig-devel
BuildRequires:  spice-protocol
BuildRequires:  nettle-devel
BuildRequires:  libXi-devel
BuildRequires:  libXinerama-devel
BuildRequires:  libXcursor-devel
BuildRequires:  libXpresent-devel
BuildRequires:  libxkbcommon-x11-devel
BuildRequires:  wayland-devel
BuildRequires:  wayland-protocols-devel
BuildRequires:  libXScrnSaver-devel
BuildRequires:  libXrandr-devel
BuildRequires:  dejavu-sans-mono-fonts
BuildRequires:  libdecor-devel
BuildRequires:  pipewire-devel
BuildRequires:  libsamplerate-devel
# cmake hard-requires libpulse even on PipeWire hosts
BuildRequires:  pulseaudio-libs-devel

%description
The Looking Glass client renders a Windows guest's GPU output relayed over a
shared-memory framebuffer (KVMFR) with SPICE input. It must be the same release
(B7) as the guest-side Looking Glass host application.

# Same source tree as the client, so the two cannot drift: `make lg-bump` moves
# both at once.
%package -n kvmfr-dkms
Summary:        KVMFR kernel module (DKMS) — DMABUF frame transfer for Looking Glass
BuildArch:      noarch
Requires:       dkms >= 3.1.8
Requires:       gcc
Requires:       make
Requires:       kernel-devel
Provides:       kvmfr = %{version}-%{release}

%description -n kvmfr-dkms
The kvmfr module exposes the Looking Glass frame buffer as a character device
that can be exported as a DMABUF, so the client's GPU reads frames directly
instead of the client copying them through an intermediate buffer. On a host
whose display runs on an iGPU this returns memory bandwidth the iGPU would
otherwise spend on that copy.

DKMS rebuilds the module on every kernel update and signs it with the host's
existing module-signing key — the same key that already signs any other DKMS
module, so Secure Boot needs no new enrollment.

%prep
%autosetup -n %{name}-%{version}

%build
# drop -Werror: Fedora's GCC raises warnings upstream pins as errors
sed -i '/^  "-Werror"$/d' client/CMakeLists.txt
# ENABLE_BACKTRACE=OFF: Fedora's static libbfd.a pulls unresolved ZSTD symbols
# OPTIMIZE_FOR_NATIVE=OFF: default -march=native bakes the COPR builder's ISA
# (AVX-512) into the binary and SIGILLs on CPUs without it; OFF selects x86-64-v2
cmake -S client -B client/build \
    -DCMAKE_BUILD_TYPE=Release \
    -DENABLE_BACKTRACE=OFF \
    -DOPTIMIZE_FOR_NATIVE=OFF
make -C client/build %{?_smp_mflags}

%install
install -Dm0755 client/build/looking-glass-client \
    %{buildroot}%{_bindir}/looking-glass-client

# The dkms tree tracks the Looking Glass release rather than the module's own
# 0.0.12, so a client bump rebuilds the module that belongs with it. Go
# addresses this tree by artifacts.LookingGlassRPMVersion.
install -d %{buildroot}%{_usrsrc}/kvmfr-%{version}
install -Dm0644 module/kvmfr.c module/kvmfr.h module/Makefile module/dkms.conf \
    -t %{buildroot}%{_usrsrc}/kvmfr-%{version}
sed -i 's/^PACKAGE_VERSION=.*/PACKAGE_VERSION="%{version}"/' \
    %{buildroot}%{_usrsrc}/kvmfr-%{version}/dkms.conf

%post -n kvmfr-dkms
dkms add -m kvmfr -v %{version} -q --rpm_safe_upgrade || :
# Build for the running kernel; 40-dkms.install covers every later one.
dkms build -m kvmfr -v %{version} -q --force || :
dkms install -m kvmfr -v %{version} -q --force || :

%preun -n kvmfr-dkms
dkms remove -m kvmfr -v %{version} -q --all --rpm_safe_upgrade || :

%files
%license LICENSE
%{_bindir}/looking-glass-client

%files -n kvmfr-dkms
%license LICENSE
%{_usrsrc}/kvmfr-%{version}/

%changelog
* Sun Jul 26 2026 Pavlo Hrytsenko <pashagricenko@gmail.com> - %{lgver}-3
- Add the kvmfr-dkms subpackage, built from the same source tree as the client.

* Tue Jul 21 2026 Pavlo Hrytsenko <pashagricenko@gmail.com> - %{lgver}-2
- Build with OPTIMIZE_FOR_NATIVE=OFF (portable x86-64-v2); -march=native
  baked AVX-512 from the COPR builder and SIGILL'd on non-AVX-512 CPUs.

* Tue Jul 21 2026 Pavlo Hrytsenko <pashagricenko@gmail.com> - %{lgver}-1
- Initial packaging.
