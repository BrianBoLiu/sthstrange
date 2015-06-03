import sys

uname = sys.argv[1]

# What's the old ledger?
old = open('LEDGER.txt', 'r').read()

new = []
newscore = None
for line in old.strip().split('\n'):
        if uname in line:
                user, score = line.split(':')
                score = int(score) + 1
                newscore = score
                line = user + ':' + str(score)
        new.append(line)
new = '\n'.join(new) + '\n'
with open('LEDGER.txt', 'w') as f:
        f.write(new)
