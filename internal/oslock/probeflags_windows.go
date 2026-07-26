package oslock

// probeOpenExtraFlags adds nothing on Windows, where O_NOFOLLOW and O_NONBLOCK are not defined. The
// Lstat check still rejects every non-regular entry.
const probeOpenExtraFlags = 0
