Name:           looking-glass-client
# version comes from the Makefile (artifacts.LookingGlassVersion)
Version:        %{lgver}
Release:        1%{?dist}
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

%prep
%autosetup -n %{name}-%{version}

%build
# drop -Werror: Fedora's GCC raises warnings upstream pins as errors
sed -i '/^  "-Werror"$/d' client/CMakeLists.txt
# ENABLE_BACKTRACE=OFF: Fedora's static libbfd.a pulls unresolved ZSTD symbols
cmake -S client -B client/build \
    -DCMAKE_BUILD_TYPE=Release \
    -DENABLE_BACKTRACE=OFF
make -C client/build %{?_smp_mflags}

%install
install -Dm0755 client/build/looking-glass-client \
    %{buildroot}%{_bindir}/looking-glass-client

%files
%license LICENSE
%{_bindir}/looking-glass-client

%changelog
* Tue Jul 21 2026 Pavlo Hrytsenko <pashagricenko@gmail.com> - %{lgver}-1
- Initial packaging.
