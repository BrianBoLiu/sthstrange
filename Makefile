# Handy flag for cuda-memcheck: compile with -lineinfo

.PHONY: sha1

all: sha1

sha1: sha1.cu libisha1.a
	nvcc $(CFLAGS) sha1.cu -L$(shell pwd) -lisha1 -o sha1

libisha1.a: isha1.o
	ar rcs libisha1.a isha1.o

isha1.o:
	nasm -f elf64 isha1.asm

clean:
	rm sha1 *.a *.o
